package system

import (
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/hblabs-co/kit/core/constants"
)

// DefaultPort is the listen port ListenAddr falls back to when
// neither HB_PORT nor Boot's Port option gives one.
const DefaultPort = 8080

// bootIdentity is what the latest Boot call was given, for helpers
// that would otherwise need the service name or port passed around.
var bootIdentity struct {
	sync.RWMutex
	service string
	port    int
}

func setBootIdentity(service string, port int) {
	bootIdentity.Lock()
	defer bootIdentity.Unlock()
	bootIdentity.service, bootIdentity.port = service, port
}

// ServiceName returns the service name given to Boot, or "" before
// Boot ran.
func ServiceName() string {
	bootIdentity.RLock()
	defer bootIdentity.RUnlock()
	return bootIdentity.service
}

// ListenPort returns the effective listen port: constants.EnvPort (HB_PORT)
// when it holds a positive integer, else the port given to Boot via
// the Port option, else DefaultPort.
func ListenPort() int {
	if v := strings.TrimSpace(os.Getenv(constants.EnvPort)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	bootIdentity.RLock()
	defer bootIdentity.RUnlock()
	if bootIdentity.port > 0 {
		return bootIdentity.port
	}
	return DefaultPort
}

// ListenAddr returns ":<ListenPort()>", the address NewServer uses
// when ServerOptions.Addr is empty.
func ListenAddr() string { return ":" + strconv.Itoa(ListenPort()) }
