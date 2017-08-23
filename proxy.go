package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"net"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/hagen1778/chproxy/config"
	"github.com/hagen1778/chproxy/log"
	"github.com/prometheus/client_golang/prometheus"
)

// Creates new reverseProxy with provided config
func NewReverseProxy(cfg *config.Config) (*reverseProxy, error) {
	rp := &reverseProxy{}
	rp.ReverseProxy = &httputil.ReverseProxy{
		Director: func(*http.Request) {},
		ErrorLog: log.ErrorLogger,
		Transport: &observableTransport{
			http.Transport{
				Proxy: http.ProxyFromEnvironment,
				DialContext: (&net.Dialer{
					KeepAlive: 30 * time.Second,
					DualStack: true,
				}).DialContext,
				MaxIdleConns:          100,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
		},
	}
	err := rp.ApplyConfig(cfg)

	return rp, err
}

// Serves incoming requests according to config
func (rp *reverseProxy) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	log.Debugf("Accepting request from %s: %s", req.RemoteAddr, req.URL.String())
	s, err := rp.getRequestScope(req)
	if err != nil {
		respondWithErr(rw, err)
		return
	}
	log.Debugf("Request scope %s", s)

	if err = s.inc(); err != nil {
		respondWithErr(rw, err)
		return
	}

	label := prometheus.Labels{
		"initial_user":   s.initialUser.name,
		"execution_user": s.executionUser.name,
		"host":           s.host.addr.Host,
	}
	requestSum.With(label).Inc()

	req.URL.Scheme = s.host.addr.Scheme
	req.URL.Host = s.host.addr.Host
	// set custom User-Agent for proper handling of killQuery func
	ua := fmt.Sprintf("ClickHouseProxy: %s", s.initialUser.name)
	req.Header.Set("User-Agent", ua)

	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	req = req.WithContext(ctx)

	c := make(chan struct{})
	go func() {
		rp.ReverseProxy.ServeHTTP(rw, req)
		c <- struct{}{}
	}()

	select {
	case <-time.After(s.initialUser.maxExecutionTime):
		cancel()
		timeouts.With(label).Inc()
		condition := fmt.Sprintf("http_user_agent = '%s'", ua)
		s.cluster.killQueries(condition, s.initialUser.maxExecutionTime.Seconds())
		message := fmt.Sprintf("timeout for initial user %q exceeded: %v", s.initialUser.name, s.initialUser.maxExecutionTime)
		rw.Write([]byte(message))
	case <-time.After(s.executionUser.maxExecutionTime):
		cancel()
		timeouts.With(label).Inc()
		condition := fmt.Sprintf("initial_user = '%s'", s.executionUser.name)
		s.cluster.killQueries(condition, s.executionUser.maxExecutionTime.Seconds())
		message := fmt.Sprintf("timeout for execution user %q exceeded: %v", s.executionUser.name, s.executionUser.maxExecutionTime)
		rw.Write([]byte(message))
	case <-c:
		requestSuccess.With(label).Inc()
	}

	s.dec()
	log.Debugf("Request scope %s successfully proxied", s)
}

// Reloads configuration from passed file
// return error if configuration is invalid
func (rp *reverseProxy) ReloadConfig(file string) error {
	cfg, err := config.LoadFile(file)
	if err != nil {
		return fmt.Errorf("can't load config %q: %s", file, err)
	}

	return rp.ApplyConfig(cfg)
}

// Applies provided config to reverseProxy
// New config will be applied only if non-nil error returned
func (rp *reverseProxy) ApplyConfig(cfg *config.Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

	rp.Lock()
	defer rp.Unlock()

	clusters := make(map[string]*cluster)
	for _, c := range cfg.Clusters {
		hosts := make([]*host, len(c.Nodes))
		for i, node := range c.Nodes {
			addr, err := url.Parse(fmt.Sprintf("%s://%s", c.Scheme, node))
			if err != nil {
				return err
			}

			hosts[i] = &host{
				addr: addr,
			}
		}

		users := make(map[string]*executionUser, len(c.OutUsers))
		for _, user := range c.OutUsers {
			users[user.Name] = &executionUser{
				name:     user.Name,
				password: user.Password,
			}
		}

		clusters[c.Name] = &cluster{
			hosts: hosts,
			users: users,
		}
	}

	initialUsers := make(map[string]*initialUser, len(cfg.GlobalUsers))
	for _, u := range cfg.GlobalUsers {
		c, ok := clusters[u.ToCluster]
		if !ok {
			return fmt.Errorf("error while mapping user %q to cluster %q: no such cluster", u.Name, u.ToCluster)
		}

		if _, ok := c.users[u.ToUser]; !ok {
			return fmt.Errorf("error while mapping user %q to cluster's %q user %q: no such user", u.Name, u.ToCluster, u.ToUser)
		}

		initialUsers[u.Name] = &initialUser{
			executionUser: executionUser{
				name:                 u.Name,
				maxConcurrentQueries: u.MaxConcurrentQueries,
				maxExecutionTime:     u.MaxExecutionTime,
			},
			toCluster: u.ToCluster,
			toUser:    u.ToUser,
		}
	}

	rp.clusters = clusters
	rp.users = initialUsers

	// Next statement looks a bit outplaced. Still don't know where it must be placed
	log.SetDebug(cfg.LogDebug)

	return nil
}

type reverseProxy struct {
	*httputil.ReverseProxy

	sync.Mutex
	users    map[string]*initialUser
	clusters map[string]*cluster
}

func (rp *reverseProxy) getRequestScope(req *http.Request) (*scope, error) {
	name, password := basicAuth(req)

	rp.Lock()
	defer rp.Unlock()

	iu, ok := rp.users[name]
	if !ok {
		return nil, fmt.Errorf("invalid username or password for user %q", name)
	}

	if iu.password != password {
		return nil, fmt.Errorf("invalid username or password for user %q", name)
	}

	c, ok := rp.clusters[iu.toCluster]
	if !ok {
		return nil, fmt.Errorf("BUG: user %q matches to unknown cluster %q", iu.name, iu.toCluster)
	}

	eu, ok := c.users[iu.toUser]
	if !ok {
		return nil, fmt.Errorf("BUG: user %q matches to unknown user %q at cluster %q", iu.name, iu.toUser, iu.toCluster)
	}

	return &scope{
		initialUser:   iu,
		executionUser: eu,
		cluster:       c,
		host:          c.getHost(),
	}, nil
}

type cluster struct {
	sync.Mutex
	hosts []*host
	users map[string]*executionUser
}

// We don't use query_id because of distributed processing, the query ID is not passed to remote servers
func (c *cluster) killQueries(condition string, elapsed float64) {
	c.Lock()
	addrs := make([]string, len(c.hosts))
	for i, host := range c.hosts {
		addrs[i] = host.addr.String()
	}
	c.Unlock()

	q := fmt.Sprintf("KILL QUERY WHERE %s AND elapsed >= %d", condition, int(elapsed))
	log.Debugf("ExecutionTime exceeded. Going to call query %q for hosts %v", q, addrs)
	for _, addr := range addrs {
		if err := doQuery(q, addr); err != nil {
			log.Errorf("error while killing queries older than %.2fs with condition %q: %s", elapsed, condition, err)
		}
	}
}

func (c *cluster) getHost() *host {
	c.Lock()
	defer c.Unlock()

	var idle *host
	for _, t := range c.hosts {
		t.Lock()
		if t.runningQueries == 0 {
			t.Unlock()
			return t
		}

		if idle == nil || idle.runningQueries > t.runningQueries {
			idle = t
		}
		t.Unlock()
	}

	return idle
}

type observableTransport struct {
	http.Transport
}

func (pt *observableTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := pt.Transport.RoundTrip(r)
	if response != nil {
		statusCodes.With(
			prometheus.Labels{"host": r.URL.Host, "code": response.Status},
		).Inc()
	}

	if err != nil {
		errors.With(
			prometheus.Labels{"host": r.URL.Host, "message": err.Error()},
		).Inc()
	}

	return response, err
}
