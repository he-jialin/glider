package rule

import (
	"hash/fnv"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nadoo/glider/log"
	"github.com/nadoo/glider/proxy"
)

// forwarder slice orderd by priority.
type priSlice []*Forwarder

func (p priSlice) Len() int           { return len(p) }
func (p priSlice) Less(i, j int) bool { return p[i].Priority() > p[j].Priority() }
func (p priSlice) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }

// FwdrGroup is a forwarder group.
type FwdrGroup struct {
	config   *Strategy
	fwdrs    priSlice
	avail    []*Forwarder // available forwarders
	mu       sync.RWMutex
	index    uint32
	priority uint32
	next     func(addr string) *Forwarder
}

// NewFwdrGroup returns a new forward group.
func NewFwdrGroup(name string, s []string, c *Strategy) *FwdrGroup {
	var fwdrs []*Forwarder
	for _, chain := range s {
		fwdr, err := ForwarderFromURL(chain, c.IntFace,
			time.Duration(c.DialTimeout)*time.Second, time.Duration(c.RelayTimeout)*time.Second)
		if err != nil {
			log.Fatal(err)
		}
		fwdr.SetMaxFailures(uint32(c.MaxFailures))
		fwdrs = append(fwdrs, fwdr)
	}

	if len(fwdrs) == 0 {
		// direct forwarder
		fwdrs = append(fwdrs, DirectForwarder(c.IntFace,
			time.Duration(c.DialTimeout)*time.Second, time.Duration(c.RelayTimeout)*time.Second))
		c.Strategy = "rr"
	}

	return newFwdrGroup(name, fwdrs, c)
}

// newFwdrGroup returns a new FwdrGroup.
func newFwdrGroup(name string, fwdrs []*Forwarder, c *Strategy) *FwdrGroup {
	p := &FwdrGroup{fwdrs: fwdrs, config: c}
	sort.Sort(p.fwdrs)

	p.init()

	// default scheduler
	p.next = p.scheduleRR

	// if there're more than 1 forwarders, we care about the strategy.
	if count := len(fwdrs); count > 1 {
		switch c.Strategy {
		case "rr":
			p.next = p.scheduleRR
			log.F("[strategy] %s: %d forwarders forward in round robin mode.", name, count)
		case "ha":
			p.next = p.scheduleHA
			log.F("[strategy] %s: %d forwarders forward in high availability mode.", name, count)
		case "lha":
			p.next = p.scheduleLHA
			log.F("[strategy] %s: %d forwarders forward in latency based high availability mode.", name, count)
		case "dh":
			p.next = p.scheduleDH
			log.F("[strategy] %s: %d forwarders forward in destination hashing mode.", name, count)
		default:
			p.next = p.scheduleRR
			log.F("[strategy] %s: not supported forward mode '%s', use round robin mode for %d forwarders.", name, c.Strategy, count)
		}
	}

	for _, f := range fwdrs {
		f.AddHandler(p.onStatusChanged)
	}

	return p
}

// Dial connects to the address addr on the network net.
func (p *FwdrGroup) Dial(network, addr string) (net.Conn, proxy.Dialer, error) {
	nd := p.NextDialer(addr)
	c, err := nd.Dial(network, addr)
	return c, nd, err
}

// DialUDP connects to the given address.
func (p *FwdrGroup) DialUDP(network, addr string) (pc net.PacketConn, writeTo net.Addr, err error) {
	return p.NextDialer(addr).DialUDP(network, addr)
}

// NextDialer returns the next dialer.
func (p *FwdrGroup) NextDialer(dstAddr string) proxy.Dialer {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if len(p.avail) == 0 {
		return p.fwdrs[atomic.AddUint32(&p.index, 1)%uint32(len(p.fwdrs))]
	}

	return p.next(dstAddr)
}

// Priority returns the active priority of dialer.
func (p *FwdrGroup) Priority() uint32 { return atomic.LoadUint32(&p.priority) }

// SetPriority sets the active priority of daler.
func (p *FwdrGroup) SetPriority(pri uint32) { atomic.StoreUint32(&p.priority, pri) }

// init traverse d.fwdrs and init the available forwarder slice.
func (p *FwdrGroup) init() {
	for _, f := range p.fwdrs {
		if f.Enabled() {
			p.SetPriority(f.Priority())
			break
		}
	}

	p.avail = nil
	for _, f := range p.fwdrs {
		if f.Enabled() && f.Priority() >= p.Priority() {
			p.avail = append(p.avail, f)
		}
	}

	if len(p.avail) == 0 {
		// no available forwarders, set priority to 0 to check all forwarders in check func
		p.SetPriority(0)
		// log.F("[group] no available forwarders, please check your config file or network settings")
	}
}

// onStatusChanged will be called when fwdr's status changed.
func (p *FwdrGroup) onStatusChanged(fwdr *Forwarder) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if fwdr.Enabled() {
		log.F("[group] %s(%d) changed status from DISABLED to ENABLED ", fwdr.Addr(), fwdr.Priority())
		if fwdr.Priority() == p.Priority() {
			p.avail = append(p.avail, fwdr)
		} else if fwdr.Priority() > p.Priority() {
			p.init()
		}
	} else {
		log.F("[group] %s(%d) changed status from ENABLED to DISABLED", fwdr.Addr(), fwdr.Priority())
		for i, f := range p.avail {
			if f == fwdr {
				p.avail[i], p.avail = p.avail[len(p.avail)-1], p.avail[:len(p.avail)-1]
				break
			}
		}
	}

	if len(p.avail) == 0 {
		p.init()
	}
}

// Check runs the forwarder checks.
func (p *FwdrGroup) Check() {
	if len(p.fwdrs) == 1 {
		p.config.MaxFailures = 0
		log.F("[group] only 1 forwarder found, disable health checking")
		return
	}

	if !strings.Contains(p.config.Check, "://") {
		p.config.Check += "://"
	}

	u, err := url.Parse(p.config.Check)
	if err != nil {
		log.F("[group] parse check config error: %s", err)
		return
	}

	addr := u.Host
	if strings.IndexByte(addr, ':') == -1 {
		addr += ":80"
	}

	timeout := time.Duration(p.config.CheckTimeout) * time.Second

	var checker Checker
	switch u.Scheme {
	case "tcp":
		checker = newTcpChecker(addr, timeout)
	case "http":
		expect := "HTTP" // default: check the first 4 chars in response
		params, _ := url.ParseQuery(u.Fragment)
		if ex := params.Get("expect"); ex != "" {
			expect = ex
		}
		checker = newHttpChecker(addr, u.RequestURI(), expect, timeout)
	case "file":
		checker = newFileChecker(u.Host + u.Path)
	default:
		p.config.MaxFailures = 0
		log.F("[group] invalid check config `%s`, disable health checking", p.config.Check)
		return
	}

	for i := 0; i < len(p.fwdrs); i++ {
		go p.check(p.fwdrs[i], checker)
	}
}

func (p *FwdrGroup) check(f *Forwarder, checker Checker) {
	wait := uint8(0)
	intval := time.Duration(p.config.CheckInterval) * time.Second

	for {
		time.Sleep(intval * time.Duration(wait))

		// check all forwarders at least one time
		if wait > 0 && (f.Priority() < p.Priority()) {
			continue
		}

		if f.Enabled() && p.config.CheckDisabledOnly {
			continue
		}

		if checker.Check(f) {
			wait = 1
			continue
		}

		if wait == 0 {
			wait = 1
		}

		wait *= 2
		if wait > 16 {
			wait = 16
		}
	}
}

// Round Robin.
func (p *FwdrGroup) scheduleRR(dstAddr string) *Forwarder {
	return p.avail[atomic.AddUint32(&p.index, 1)%uint32(len(p.avail))]
}

// High Availability.
func (p *FwdrGroup) scheduleHA(dstAddr string) *Forwarder {
	return p.avail[0]
}

// Latency based High Availability.
func (p *FwdrGroup) scheduleLHA(dstAddr string) *Forwarder {
	oldfwdr, newfwdr := p.avail[0], p.avail[0]
	lowest := oldfwdr.Latency()
	for _, f := range p.avail {
		if f.Latency() < lowest {
			lowest = f.Latency()
			newfwdr = f
		}
	}
	tolerance := int64(p.config.CheckTolerance) * int64(time.Millisecond)
	if newfwdr.Latency() < (oldfwdr.Latency() - tolerance) {
		return newfwdr
	}
	return oldfwdr
}

// Destination Hashing.
func (p *FwdrGroup) scheduleDH(dstAddr string) *Forwarder {
	fnv1a := fnv.New32a()
	fnv1a.Write([]byte(dstAddr))
	return p.avail[fnv1a.Sum32()%uint32(len(p.avail))]
}
