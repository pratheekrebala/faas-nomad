package resolver

import (
	"fmt"
	"github.com/hashicorp/consul-template/dependency"
	"github.com/hashicorp/consul-template/watch"
	"github.com/hashicorp/consul/agent/connect"
	"github.com/hashicorp/go-hclog"
	"github.com/jsiebens/faas-nomad/pkg/types"
	"math/rand"
	"net/url"
	"strings"
	"sync"
	"time"
)

type ServiceResolver interface {
	Resolve(functionName string) (string, connect.CertURI, error)
	ResolveAll(functionName string) ([]string, connect.CertURI, error)
}

type ConsulServiceResolver struct {
	clientSet *dependency.ClientSet
	watcher   *watch.Watcher
	cache     sync.Map
	prefix    string
	namespace string
	connect   bool
	logger    hclog.Logger
}

type serviceItem struct {
	serviceQuery dependency.Dependency
	certURI      connect.CertURI
	addresses    []string
}

func NewConsulResolver(config *types.ProviderConfig, logger hclog.Logger) (ServiceResolver, error) {
	clientSet := dependency.NewClientSet()
	err := clientSet.CreateConsulClient(&dependency.CreateConsulClientInput{
		Address:    config.Consul.Addr,
		Token:      config.Consul.ACLToken,
		SSLEnabled: strings.HasPrefix(config.Consul.Addr, "https"),
		SSLCACert:  config.Consul.CACert,
		SSLCert:    config.Consul.ClientCert,
		SSLKey:     config.Consul.ClientKey,
		SSLVerify:  !config.Consul.TLSSkipVerify,
	})

	if err != nil {
		return nil, err
	}

	watcher, _ := watch.NewWatcher(&watch.NewWatcherInput{
		Clients:  clientSet,
		MaxStale: 10000 * time.Millisecond,
	})

	resolver := &ConsulServiceResolver{
		clientSet: clientSet,
		watcher:   watcher,
		prefix:    config.Scheduling.JobPrefix,
		namespace: config.Scheduling.Namespace,
		connect:   config.Consul.ConnectAware,
		logger:    logger,
	}

	go resolver.watch()
	go resolver.reset()

	return resolver, nil
}

func (cr *ConsulServiceResolver) reset() {
	ticker := time.NewTicker(time.Duration(30) * time.Minute)

	for range ticker.C {
		cr.watcher.Stop()

		watcher, _ := watch.NewWatcher(&watch.NewWatcherInput{
			Clients:  cr.clientSet,
			MaxStale: 10000 * time.Millisecond,
		})

		cr.cache = sync.Map{}
		cr.watcher = watcher
	}
}

func (cr *ConsulServiceResolver) ResolveAll(function string) ([]string, connect.CertURI, error) {
	return cr.resolveInternal(fmt.Sprintf("%s%s", cr.prefix, strings.TrimSuffix(function, "."+cr.namespace)))
}

func (cr *ConsulServiceResolver) Resolve(function string) (string, connect.CertURI, error) {
	candidates, certURI, err := cr.ResolveAll(function)
	if err != nil {
		return "", nil, err
	}
	return balance(candidates, certURI)
}

func (cr *ConsulServiceResolver) resolveInternal(service string) ([]string, connect.CertURI, error) {
	query, err := cr.healthServiceQuery(service)
	if err != nil {
		return nil, nil, err
	}

	// Generate the expected CertURI
	certURI := &connect.SpiffeIDService{
		// No host since we don't validate trust domain here (we rely on x509 to
		// prove trust).
		Namespace:  cr.namespace,
		Datacenter: "dc1", // TODO
		Service:    service,
	}

	if val, ok := cr.cache.Load(query.String()); ok {
		return val.(*serviceItem).addresses, certURI, nil
	}

	fetch, _, err := query.Fetch(cr.clientSet, nil)
	if err != nil {
		return nil, nil, err
	}

	services := fetch.([]*dependency.HealthService)
	item := cr.updateCatalog(query, services)

	_, _ = cr.watcher.Add(query)

	return item.addresses, certURI, nil
}

func (cr *ConsulServiceResolver) healthServiceQuery(service string) (*dependency.HealthServiceQuery, error) {
	if cr.connect {
		return dependency.NewHealthConnectQuery(service)
	} else {
		return dependency.NewHealthServiceQuery(service)
	}
}

func (cr *ConsulServiceResolver) updateCatalog(dep dependency.Dependency, services []*dependency.HealthService) *serviceItem {
	addresses := make([]string, 0)

	for _, s := range services {
		if len(s.Checks) > 1 {
			addresses = append(addresses, fmt.Sprintf("%v:%v", s.Address, s.Port))
		}
	}

	item := &serviceItem{
		serviceQuery: dep,
		addresses:    addresses,
	}

	cr.cache.Store(dep.String(), item)

	return item
}

func (cr *ConsulServiceResolver) watch() {
	for d := range cr.watcher.DataCh() {
		cr.updateCatalog(d.Dependency(), d.Data().([]*dependency.HealthService))
	}
}

func balance(candidates []string, certURI connect.CertURI) (string, connect.CertURI, error) {
	if candidates == nil || len(candidates) == 0 {
		return "", nil, fmt.Errorf("no candidate available")
	}
	idx := 0
	if len(candidates) > 1 {
		idx = rand.Intn(len(candidates))
	}
	return candidates[idx], certURI, nil
}

func toUrl(address string, port int) url.URL {
	parse, _ := url.Parse(fmt.Sprintf("http://%v:%v", address, port))
	return *parse
}
