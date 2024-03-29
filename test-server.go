package redisemu

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jimsnab/go-lane"
)

type (
	RedisEmu struct {
		mu       sync.Mutex
		l        lane.Lane
		dss      *dataStoreSet
		server   net.Listener
		cancelFn context.CancelFunc
		wg       sync.WaitGroup
		hook     DispatchHook

		port            int
		iface           string
		persistBasePath string
		quitSignal      chan struct{}

		disableClientSetInfo bool // special flag for redis client issue
	}
)

func NewEmulator(l lane.Lane, port int, iface string, persistBasePath string, quitSignal chan struct{}) (eng *RedisEmu, err error) {
	l2, cancelFn := l.DeriveWithCancel()

	eng = &RedisEmu{
		l:               l2,
		cancelFn:        cancelFn,
		port:            port,
		iface:           iface,
		persistBasePath: persistBasePath,
		quitSignal:      quitSignal,
	}

	return
}

// special function for testing redis client library - this must be called before Start
func (eng *RedisEmu) DisableClientSetInfo() {
	eng.disableClientSetInfo = true
}

func (eng *RedisEmu) Port() int {
	return eng.port
}

func (eng *RedisEmu) NetInterface() string {
	return eng.iface
}

func (eng *RedisEmu) Start() {
	if eng.quitSignal != nil {
		fmt.Printf("\r\n\r\nREDIS Emulator is now running\r\n\r\nPress any key to quit\r\n\r\n")
	}

	eng.dss = newDataStoreSet(eng.l, eng.persistBasePath, &eng.hook)

	// launch termination monitiors
	eng.killSignalMonitor()

	if eng.quitSignal != nil {
		eng.exitKeyMonitor()
	}

	// launch periodic save goroutine
	eng.periodicSave()

	// start accepting connections and processing them
	eng.startServer()
}

func (eng *RedisEmu) RequestTermination() {
	eng.mu.Lock()
	defer eng.mu.Unlock()

	if eng.server != nil {
		// the only way to stop the blocking listen is to close its connection
		eng.server.Close()
		eng.server = nil
	}

	if eng.cancelFn != nil {
		eng.cancelFn()
		eng.cancelFn = nil
	}
}

func (eng *RedisEmu) killSignalMonitor() {
	// register a graceful termination handler
	sigs := make(chan os.Signal, 10)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)

	eng.wg.Add(1)
	go func() {
		defer eng.wg.Done()

		eng.l.Trace("kill signal monitor running")

		select {
		case sig := <-sigs:
			eng.l.Debugf("kill signal received: %s", sig.String())
			eng.RequestTermination()
			return

		case <-eng.l.Done():
			eng.l.Debug("kill monitor canceled")
			return
		}
	}()
}

func (eng *RedisEmu) exitKeyMonitor() {
	// Start a go routine to detect a keypress. Upon termination
	// triggered another way, this goroutine will leak. Go does
	// not give a reasonable way to cancel a blocking I/O call.
	eng.wg.Add(1)
	go func() {
		defer eng.wg.Done()

		eng.l.Trace("exit key monitor running")
		select {
		case <-eng.l.Done():
			eng.l.Debug("exit key monitor canceled")
			break
		case <-eng.quitSignal:
			eng.l.Debug("exit key monitor signaled")
			eng.RequestTermination()
			break
		}
	}()
}

func (eng *RedisEmu) periodicSave() {
	// make a periodic save that will also ensure save upon termination
	if eng.dss.basePath != "" {
		eng.wg.Add(1)
		go func() {
			defer eng.wg.Done()

			timer := time.NewTicker(time.Second)
			for {
				select {
				case <-eng.l.Done():
					eng.l.Debug("saver loop canceled")
					timer.Stop()
					eng.dss.save(eng.l)
					return
				case <-timer.C:
					eng.dss.save(eng.l)
				}
			}
		}()
	}
}

func (eng *RedisEmu) startServer() {
	// establish socket service
	var err error

	if eng.iface == "" {
		eng.iface = fmt.Sprintf(":%d", eng.port)
	} else {
		eng.iface = fmt.Sprintf("%s:%d", eng.iface, eng.port)
	}
	server, err := net.Listen("tcp", eng.iface)
	if err != nil {
		fmt.Println("Error listening: ", err.Error())
		os.Exit(1)
	}

	eng.server = server
	eng.l.Infof("listening on %s", server.Addr().String())

	// make a command dispatcher
	rd := newRespDeserializerFromResource(eng.l, cmdSpec)
	value, _, valid := rd.deserializeNext()
	if !valid {
		eng.l.Fatal("invalid command definition content")
	}

	cmds := redisCommands{}
	if valid = cmds.respDeserialize(eng.l, value); !valid {
		eng.l.Fatal("failed to deserialize command definitions")
	}

	ri := newRespDeserializerFromResource(eng.l, cmdInfoSpec)
	value, _, valid = ri.deserializeNext()
	if !valid {
		eng.l.Warnf("command info definition error at pos %d line %d", ri.pos, ri.lineNumber)
		eng.l.Fatal("invalid command definition content")
	}

	info := newRedisInfoTable()
	if valid = info.respDeserialize(eng.l, value); !valid {
		eng.l.Fatal("failed to deserialize command info definitions")
	}

	dispatcher := newCmdDispatcher(eng.port, eng.iface, cmds, info, eng.dss)
	if eng.disableClientSetInfo {
		dispatcher.disableCmd("client|setinfo")
	}

	eng.wg.Add(1)
	go func() {
		defer eng.wg.Done()

		// accept connections and process commands
		for {
			connection, err := server.Accept()
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					eng.l.Errorf("accept error: %s", err)
				}
				break
			}
			eng.l.Infof("client connected: %s", connection.RemoteAddr().String())
			newClientCxn(eng.l, connection, dispatcher)
		}
	}()
}

func (eng *RedisEmu) WaitForTermination() {
	// wait for server to quiesque
	eng.wg.Wait()
	eng.l.Info("finished serving requests")
}

// Calls RequestTermination then WaitForTermination
func (eng *RedisEmu) Close() {
	eng.RequestTermination()
	eng.WaitForTermination()
}

func (eng *RedisEmu) SetHook(hook DispatchHook) {
	eng.mu.Lock()
	defer eng.mu.Unlock()

	eng.hook = hook
}
