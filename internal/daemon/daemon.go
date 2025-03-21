package daemon

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"

	"applock-go/internal/config"
	"applock-go/internal/ipc"
	"applock-go/internal/logging"
	"applock-go/internal/monitor"
	"applock-go/internal/privilege"
	"applock-go/internal/util"
)

// Daemon represents the privileged process monitoring daemon
type Daemon struct {
	config          *config.Config
	monitor         *monitor.ProcessMonitor
	socket          net.Listener
	logger          *logging.Logger
	connections     map[net.Conn]struct{}
	stopCh          chan struct{}
	shutdownHandler *util.ShutdownHandler
	privManager     *privilege.PrivilegeManager
}

// NewDaemon creates a new privileged daemon
func NewDaemon(cfg *config.Config) (*Daemon, error) {
	logger := logging.NewLogger("[daemon]", cfg.Verbose)

	// Create privilege manager
	privManager, err := privilege.NewPrivilegeManager(logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create privilege manager: %w", err)
	}

	// Create process monitor without authenticator - authentication
	// will be handled by the unprivileged client
	monitor, err := monitor.NewProcessMonitorDaemon(cfg, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create process monitor: %w", err)
	}

	daemon := &Daemon{
		config:      cfg,
		monitor:     monitor,
		logger:      logger,
		connections: make(map[net.Conn]struct{}),
		stopCh:      make(chan struct{}),
		privManager: privManager,
	}

	// Create shutdown handler
	daemon.shutdownHandler = util.NewShutdownHandler(logger, 10*time.Second)

	// Register shutdown functions
	daemon.shutdownHandler.RegisterShutdownFunc(func() error {
		return daemon.Stop()
	})

	// Register process cleanup function that ensures no suspended processes remain
	daemon.shutdownHandler.RegisterShutdownFunc(util.CheckProcessesBeforeExit)

	return daemon, nil
}

// Start begins the daemon and listens for client connections
func (d *Daemon) Start() error {
	// Setup socket for IPC
	socketPath := "/var/run/applock-daemon.sock"

	// Remove existing socket if it exists
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove existing socket: %w", err)
	}

	// Create socket
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("failed to create socket: %w", err)
	}
	d.socket = listener

	// Set permissions so non-root can connect
	if err := os.Chmod(socketPath, 0666); err != nil {
		return fmt.Errorf("failed to set socket permissions: %w", err)
	}

	// Start process monitor
	if err := d.monitor.Start(); err != nil {
		return fmt.Errorf("failed to start monitor: %w", err)
	}

	// Drop privileges while maintaining required capabilities
	if err := d.privManager.DropPrivileges(); err != nil {
		return fmt.Errorf("failed to drop privileges: %w", err)
	}

	// Begin handling shutdown signals
	d.shutdownHandler.HandleShutdown()

	// Accept and handle client connections
	go d.acceptConnections()

	d.logger.Info("Daemon started successfully")
	return nil
}

// acceptConnections handles incoming client connections
func (d *Daemon) acceptConnections() {
	for {
		conn, err := d.socket.Accept()
		if err != nil {
			select {
			case <-d.stopCh:
				return // Shutdown in progress
			default:
				d.logger.Errorf("Failed to accept connection: %v", err)
				continue
			}
		}

		// Register connection
		d.connections[conn] = struct{}{}

		// Handle client in a goroutine
		go d.handleClient(conn)
	}
}

// handleClient processes messages from a connected client
func (d *Daemon) handleClient(conn net.Conn) {
	defer func() {
		conn.Close()
		delete(d.connections, conn)
	}()

	decoder := json.NewDecoder(conn)
	encoder := json.NewEncoder(conn)

	// Set a read deadline to prevent hanging
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))

	for {
		var msg ipc.Message
		if err := decoder.Decode(&msg); err != nil {
			d.logger.Debugf("Client disconnected: %v", err)
			return
		}

		// Reset read deadline for active connections
		conn.SetReadDeadline(time.Now().Add(30 * time.Second))

		switch msg.Type {
		case ipc.MsgPing:
			// Respond to ping
			encoder.Encode(ipc.Message{
				Type: ipc.MsgPong,
			})

		case ipc.MsgAuthResponse:
			// Client is responding to an auth request
			if msg.Success {
				// Auth successful, resume the process
				if err := d.monitor.ResumeProcess(msg.PID); err != nil {
					d.logger.Errorf("Failed to resume process %d: %v", msg.PID, err)
				}
			} else {
				// Auth failed, terminate the process
				if err := d.monitor.TerminateProcess(msg.PID); err != nil {
					d.logger.Errorf("Failed to terminate process %d: %v", msg.PID, err)
				}
			}

		case ipc.MsgShutdown:
			// Client requested shutdown
			d.logger.Info("Shutdown requested by client")
			d.Stop()
			return
		}
	}
}

// RegisterProcessEventHandler registers a callback for process events
func (d *Daemon) RegisterProcessEventHandler() {
	d.monitor.RegisterEventHandler(func(pid int, execPath string, displayName string) {
		// Create process event message
		msg := ipc.Message{
			Type: ipc.MsgProcessEvent,
			Process: &monitor.ProcessInfo{
				PID:     pid,
				Command: execPath,
				Allowed: false,
			},
			AppName: displayName,
		}

		// Broadcast to all clients
		d.broadcastMessage(msg)
	})
}

// broadcastMessage sends a message to all connected clients
func (d *Daemon) broadcastMessage(msg ipc.Message) {
	for conn := range d.connections {
		encoder := json.NewEncoder(conn)
		if err := encoder.Encode(msg); err != nil {
			d.logger.Debugf("Failed to send message to client: %v", err)
			// Remove failed connection
			conn.Close()
			delete(d.connections, conn)
		}
	}
}

// Stop gracefully shuts down the daemon
func (d *Daemon) Stop() error {
	close(d.stopCh)

	// Restore privileges for cleanup
	if err := d.privManager.RestorePrivileges(); err != nil {
		d.logger.Errorf("Error restoring privileges: %v", err)
	}

	// Stop the monitor
	if err := d.monitor.Stop(); err != nil {
		d.logger.Errorf("Error stopping monitor: %v", err)
	}

	// Close all client connections
	for conn := range d.connections {
		if err := conn.Close(); err != nil {
			d.logger.Errorf("Error closing client connection: %v", err)
		}
	}
	d.connections = make(map[net.Conn]struct{})

	// Close the socket
	if d.socket != nil {
		if err := d.socket.Close(); err != nil {
			d.logger.Errorf("Error closing socket: %v", err)
		}
	}

	d.logger.Info("Daemon stopped successfully")
	return nil
}
