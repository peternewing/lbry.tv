package sdkrouter

import (
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/lbryio/lbrytv/internal/metrics"
	"github.com/lbryio/lbrytv/internal/monitor"
	"github.com/lbryio/lbrytv/models"

	ljsonrpc "github.com/lbryio/lbry.go/v2/extras/jsonrpc"
)

const RPCTimeout = 300 * time.Second

var logger = monitor.NewModuleLogger("sdkrouter")

func DisableLogger() { logger.Disable() } // for testing

type Router struct {
	mu      sync.RWMutex
	servers []*models.LbrynetServer

	loadMu sync.RWMutex
	load   map[*models.LbrynetServer]uint64

	useDB      bool
	lastLoaded time.Time
}

func New(servers map[string]string) *Router {
	if len(servers) > 0 {
		s := make([]*models.LbrynetServer, len(servers))
		i := 0
		for name, address := range servers {
			s[i] = &models.LbrynetServer{Name: name, Address: address}
			i++
		}
		return NewWithServers(s...)
	}

	r := &Router{
		load:  make(map[*models.LbrynetServer]uint64),
		useDB: true,
	}
	r.reloadServersFromDB()
	return r
}

func NewWithServers(servers ...*models.LbrynetServer) *Router {
	r := &Router{
		load: make(map[*models.LbrynetServer]uint64),
	}
	r.setServers(servers)
	return r
}

func (r *Router) GetAll() []*models.LbrynetServer {
	r.reloadServersFromDB()
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.servers
}

func (r *Router) RandomServer() *models.LbrynetServer {
	r.reloadServersFromDB()
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.servers[rand.Intn(len(r.servers))]
}

func (r *Router) reloadServersFromDB() {
	if !r.useDB || time.Since(r.lastLoaded) < 30*time.Second {
		// don't hammer the DB
		return
	}

	r.lastLoaded = time.Now()
	servers, err := models.LbrynetServers().AllG()
	if err != nil {
		logger.Log().Error("Error retrieving lbrynet servers: ", err)
	}

	r.setServers(servers)
}

func (r *Router) setServers(servers []*models.LbrynetServer) {
	if len(servers) == 0 {
		logger.Log().Error("Setting servers to empty list")
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.servers = servers
	logger.Log().Debugf("updated server list to %d servers", len(r.servers))
}

// WatchLoad keeps updating the metrics on the number of wallets loaded for each instance
func (r *Router) WatchLoad() {
	ticker := time.NewTicker(2 * time.Minute)

	logger.Log().Infof("SDK router watching load on %d instances", len(r.servers))
	r.reloadServersFromDB()
	r.updateLoadAndMetrics()

	time.Sleep(time.Duration(rand.Intn(60)) * time.Second) // stagger these so they don't all happen at the same time for every api server

	for {
		<-ticker.C
		r.reloadServersFromDB()
		r.updateLoadAndMetrics()
	}
}

func (r *Router) updateLoadAndMetrics() {
	for _, server := range r.GetAll() {
		metric := metrics.LbrynetWalletsLoaded.WithLabelValues(server.Address)
		walletList, err := ljsonrpc.NewClient(server.Address).WalletList("", 1, 1)
		if err != nil {
			logger.Log().Errorf("lbrynet instance %s is not responding: %v", server.Address, err)
			r.loadMu.Lock()
			delete(r.load, server)
			r.loadMu.Unlock()
			metric.Set(-1.0)
			// TODO: maybe mark this instance as unresponsive so new users are assigned to other instances
		} else {
			r.loadMu.Lock()
			r.load[server] = walletList.TotalPages
			r.loadMu.Unlock()
			metric.Set(float64(walletList.TotalPages))
		}
	}
	leastLoaded := r.LeastLoaded()
	logger.Log().Infof("After updating load, least loaded server is %s", leastLoaded.Address)
}

// LeastLoaded returns the least-loaded wallet
func (r *Router) LeastLoaded() *models.LbrynetServer {
	var best *models.LbrynetServer
	var min uint64

	r.loadMu.RLock()
	defer r.loadMu.RUnlock()

	if len(r.load) == 0 {
		logger.Log().Warnf("LeastLoaded() called before load metrics were updated. Returning random server.")
		return r.RandomServer()
	}

	for server, numWallets := range r.load {
		logger.Log().Debugf("load update: considering %s with load %d", server.Address, numWallets)
		if best == nil || numWallets < min {
			logger.Log().Debugf("load update: %s has least with %d", server.Address, numWallets)
			best = server
			min = numWallets
		}
	}

	return best
}

// WalletID formats user ID to use as an LbrynetServer wallet ID.
func WalletID(userID int) string {
	// warning: changing this template will require renaming the stored wallet files in lbrytv
	const template = "lbrytv-id.%d.wallet"
	return fmt.Sprintf(template, userID)
}
