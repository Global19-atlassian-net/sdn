package common

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/miekg/dns"

	"k8s.io/apimachinery/pkg/util/sets"
	utilnet "k8s.io/utils/net"
)

const (
	// defaultTTL the time (in seconds) used as a TTL if an invalid or zero TTL is provided.
	defaultTTL = 30 * 60
)

type dnsValue struct {
	// All IP addresses for a given domain name
	ips []net.IP
	// Time-to-live value from non-authoritative/cached name server for the domain
	ttl time.Duration
	// Holds (last dns lookup time + ttl), tells when to refresh IPs next time
	nextQueryTime time.Time
}

type DNS struct {
	// Protects dnsMap operations
	lock sync.Mutex
	// Holds DNS name and its corresponding information
	dnsMap map[string]dnsValue

	// DNS resolvers, as host:port
	nameservers []string

	// IP Families to return results for
	ipv4 bool
	ipv6 bool

	// query timeout; overridden by tests
	timeout time.Duration
}

func NewDNS(resolverConfigFile string, ipv4, ipv6 bool) (*DNS, error) {
	if !ipv4 && !ipv6 {
		return nil, fmt.Errorf("must support at least one of IPv4 or IPv6")
	}

	config, err := dns.ClientConfigFromFile(resolverConfigFile)
	if err != nil || config == nil {
		return nil, fmt.Errorf("cannot initialize the resolver: %v", err)
	}

	return &DNS{
		dnsMap:      map[string]dnsValue{},
		nameservers: fixupNameservers(config.Servers, config.Port, ipv4, ipv6),
		ipv4:        ipv4,
		ipv6:        ipv6,
		timeout:     5 * time.Second,
	}, nil
}

func (d *DNS) Size() int {
	d.lock.Lock()
	defer d.lock.Unlock()

	return len(d.dnsMap)
}

func (d *DNS) Get(dns string) dnsValue {
	d.lock.Lock()
	defer d.lock.Unlock()

	data := dnsValue{}
	if res, ok := d.dnsMap[dns]; ok {
		data.ips = make([]net.IP, len(res.ips))
		copy(data.ips, res.ips)
		data.ttl = res.ttl
		data.nextQueryTime = res.nextQueryTime
	}
	return data
}

func (d *DNS) Add(dns string) error {
	d.lock.Lock()
	defer d.lock.Unlock()

	d.dnsMap[dns] = dnsValue{}
	_, err := d.updateOne(dns)
	if err != nil {
		delete(d.dnsMap, dns)
	}
	return err
}

func (d *DNS) Delete(dns string) {
	d.lock.Lock()
	defer d.lock.Unlock()
	delete(d.dnsMap, dns)
}

func (d *DNS) Update(dnsName string) (bool, error) {
	d.lock.Lock()
	defer d.lock.Unlock()

	return d.updateOne(dnsName)
}

func (d *DNS) updateOne(dns string) (bool, error) {
	res, ok := d.dnsMap[dns]
	if !ok {
		// Should not happen, all operations on dnsMap are synchronized by d.lock
		return false, fmt.Errorf("DNS value not found in dnsMap for domain: %q", dns)
	}

	ips, ttl, err := d.getIPsAndMinTTL(dns)
	if err != nil {
		res.nextQueryTime = time.Now().Add(defaultTTL * time.Second)
		d.dnsMap[dns] = res
		return false, err
	}

	changed := false
	if !ipsEqual(res.ips, ips) {
		changed = true
	}
	res.ips = ips
	res.ttl = ttl
	res.nextQueryTime = time.Now().Add(res.ttl)
	d.dnsMap[dns] = res
	return changed, nil
}

func (d *DNS) doOneQuery(server, domain string, rtype uint16) ([]net.IP, int, error) {
	ips := []net.IP{}
	ttl := defaultTTL

	msg := new(dns.Msg)
	msg.SetQuestion(dns.Fqdn(domain), rtype)

	c := new(dns.Client)
	c.Timeout = d.timeout
	in, _, err := c.Exchange(msg, server)
	if in == nil || err != nil {
		return ips, ttl, err
	}
	if in.Rcode != dns.RcodeSuccess {
		return ips, ttl, fmt.Errorf("failed to get a valid answer: %v", in)
	}

	for _, a := range in.Answer {
		aTTL := int(a.Header().Ttl)
		if aTTL < ttl && aTTL != 0 {
			ttl = aTTL
		}

		switch t := a.(type) {
		case *dns.A:
			if rtype == dns.TypeA {
				ips = append(ips, t.A)
			}
		case *dns.AAAA:
			if rtype == dns.TypeAAAA {
				ips = append(ips, t.AAAA)
			}
		}
	}

	return ips, ttl, nil
}

func (d *DNS) queryServer(nameserver, domain string) ([]net.IP, int, error) {
	if d.ipv4 && !d.ipv6 {
		// Single-stack IPv4
		return d.doOneQuery(nameserver, domain, dns.TypeA)
	} else if d.ipv6 && !d.ipv4 {
		// Single-stack IPv6
		return d.doOneQuery(nameserver, domain, dns.TypeAAAA)
	}
	// else dual stack
	ips := []net.IP{}
	ttl := defaultTTL
	errs := make(chan error)
	var mutex sync.Mutex

	go func() {
		v4ips, v4ttl, v4err := d.doOneQuery(nameserver, domain, dns.TypeA)
		mutex.Lock()
		defer mutex.Unlock()
		ips = append(ips, v4ips...)
		if v4ttl < ttl {
			ttl = v4ttl
		}
		errs <- v4err
	}()
	go func() {
		v6ips, v6ttl, v6err := d.doOneQuery(nameserver, domain, dns.TypeAAAA)
		mutex.Lock()
		defer mutex.Unlock()
		ips = append(ips, v6ips...)
		if v6ttl < ttl {
			ttl = v6ttl
		}
		errs <- v6err
	}()

	err1 := <-errs
	err2 := <-errs
	if len(ips) > 0 {
		return ips, ttl, nil
	} else if err1 != nil {
		return ips, ttl, err1
	} else {
		return ips, ttl, err2
	}
}

func (d *DNS) getIPsAndMinTTL(domain string) ([]net.IP, time.Duration, error) {
	var ips []net.IP
	var ttl int
	var err error

	for _, server := range d.nameservers {
		ips, ttl, err = d.queryServer(server, domain)
		if len(ips) > 0 {
			break
		}
	}

	if len(ips) == 0 {
		if err != nil {
			return nil, defaultTTL, fmt.Errorf("IP address not found for domain %q: %v", domain, err)
		} else {
			return nil, defaultTTL, fmt.Errorf("IP address not found for domain %q", domain)
		}
	}
	return removeDuplicateIPs(ips), time.Duration(ttl) * time.Second, nil
}

func (d *DNS) GetNextQueryTime() (time.Time, string, bool) {
	d.lock.Lock()
	defer d.lock.Unlock()

	timeSet := false
	var minTime time.Time
	var dns string

	for dnsName, res := range d.dnsMap {
		if (timeSet == false) || res.nextQueryTime.Before(minTime) {
			timeSet = true
			minTime = res.nextQueryTime
			dns = dnsName
		}
	}

	return minTime, dns, timeSet
}

func ipsEqual(oldips, newips []net.IP) bool {
	if len(oldips) != len(newips) {
		return false
	}

	for _, oldip := range oldips {
		found := false
		for _, newip := range newips {
			if oldip.Equal(newip) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// fixupNameservers ensures that each nameserver has an associated port number, and removes
// nameservers that don't match the cluster address family (unless that would leave us with
// no nameservers).
func fixupNameservers(nameservers []string, defaultPort string, ipv4, ipv6 bool) []string {
	// ipSupported maps from the return value of utilnet.IsIPv6String() to whether we support it
	ipSupported := map[bool]bool{false: ipv4, true: ipv6}

	var goodServers, badServers []string
	for _, server := range nameservers {
		ipString := server
		if host, _, err := net.SplitHostPort(server); err == nil {
			ipString = host
		} else {
			server = net.JoinHostPort(server, defaultPort)
		}

		if ipSupported[utilnet.IsIPv6String(ipString)] {
			goodServers = append(goodServers, server)
		} else {
			badServers = append(badServers, server)
		}
	}

	if len(goodServers) > 0 {
		return goodServers
	} else {
		return badServers
	}
}

func removeDuplicateIPs(ips []net.IP) []net.IP {
	ipSet := sets.NewString()
	for _, ip := range ips {
		ipSet.Insert(ip.String())
	}

	uniqueIPs := []net.IP{}
	for _, str := range ipSet.List() {
		ip := net.ParseIP(str)
		if ip != nil {
			uniqueIPs = append(uniqueIPs, ip)
		}
	}

	return uniqueIPs
}
