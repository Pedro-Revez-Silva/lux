package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/marcioapm/lux/internal/proto"
)

func (a *app) execCmd() *cobra.Command {
	var tty, noTTY bool
	cmd := &cobra.Command{
		Use:   "exec <run> -- command...",
		Short: "Run a command inside a Run's container",
		Long: `Run a command inside a running Run's container, as its workload user,
with its environment. Standard input, output and the exit code are the
command's. A terminal is allocated when stdin is one (-t forces it, -T turns
it off).`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			useTTY := (tty || a.stdinTerminal()) && !noTTY
			return a.exec(ctxOf(cmd), args[0], proto.StreamOpen{Command: args[1:], TTY: useTTY}, useTTY)
		},
	}
	cmd.Flags().BoolVarP(&tty, "tty", "t", false, "allocate a terminal")
	cmd.Flags().BoolVarP(&noTTY, "no-tty", "T", false, "do not allocate a terminal")
	return cmd
}

func (a *app) attachCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "attach <run>",
		Short: "Attach to a generic workload's terminal",
		Long: `Attach to the terminal of a generic workload started with workload.tty:
see its output from now on and type into it. Detach with Ctrl-] (the
workload keeps running).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := ctxOf(cmd)
			ws, err := a.c.Dial(ctx, "/v1/runs/"+args[0]+"/attach")
			if err != nil {
				return err
			}
			defer ws.CloseNow()
			return a.interact(ctx, ws, a.stdinTerminal())
		},
	}
}

// exec runs open's command in a Run and interacts with it (raw: stdin as
// a raw terminal, whose size the command's terminal starts with).
func (a *app) exec(ctx context.Context, run string, open proto.StreamOpen, raw bool) error {
	ws, err := a.c.Dial(ctx, "/v1/runs/"+run+"/exec")
	if err != nil {
		return err
	}
	defer ws.CloseNow()
	if raw {
		open.Cols, open.Rows, _ = term.GetSize(int(os.Stdout.Fd()))
	}
	if err := wsjson.Write(ctx, ws, open); err != nil {
		return err
	}
	return a.interact(ctx, ws, raw)
}

// stdinTerminal: whether the CLI's stdin is a terminal (raw mode, sizes).
func (a *app) stdinTerminal() bool {
	f, ok := a.stdin.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// interact connects stdin and stdout to a stream until it ends, and
// returns the remote exit code as the command's.
func (a *app) interact(ctx context.Context, ws *websocket.Conn, raw bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if raw {
		fd := int(a.stdin.(*os.File).Fd())
		if st, err := term.MakeRaw(fd); err == nil {
			defer term.Restore(fd, st)
		}
		// Window size changes follow the local terminal.
		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		defer signal.Stop(winch)
		go func() {
			for range winch {
				if c, r, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
					_ = wsjson.Write(ctx, ws, proto.StreamData{Rows: r, Cols: c})
				}
			}
		}()
	}
	go func() {
		pump(a.stdin, func(b []byte) bool {
			// Ctrl-] detaches from a terminal session.
			if raw && len(b) == 1 && b[0] == 0x1d {
				cancel()
				return false
			}
			return wsjson.Write(ctx, ws, proto.StreamData{Data: b}) == nil
		})
		_ = wsjson.Write(ctx, ws, proto.StreamData{EOF: true})
	}()
	for {
		var d proto.StreamData
		if err := wsjson.Read(ctx, ws, &d); err != nil {
			if ctx.Err() != nil || websocket.CloseStatus(err) == websocket.StatusNormalClosure {
				return nil // detached, or the stream just ended
			}
			return fmt.Errorf("stream ended: %w", err)
		}
		switch {
		case d.Error != "":
			return fmt.Errorf("%s", d.Error)
		case d.ExitCode != nil:
			if *d.ExitCode != 0 {
				return exitCode(*d.ExitCode)
			}
			return nil
		case d.Channel == "stderr":
			_, _ = a.stderr.Write(d.Data)
		default:
			_, _ = a.stdout.Write(d.Data)
		}
	}
}

func (a *app) portForwardCmd() *cobra.Command {
	var address string
	cmd := &cobra.Command{
		Use:   "port-forward <run> <port-name> <local-port>",
		Short: "Forward a local port to a Run's declared port",
		Long: `Listen on a local port and tunnel each connection, through luxd, to a
port the Run declares in network.ports. Runs until interrupted.`,
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := ctxOf(cmd)
			path := "/v1/runs/" + args[0] + "/ports/" + args[1]
			// Check before listening: a wrong name or a stopped Run fails
			// now (the same URL, without the upgrade, only checks).
			if err := a.c.Do(ctx, "GET", path, nil, nil); err != nil {
				return err
			}
			ln, err := net.Listen("tcp", net.JoinHostPort(address, args[2]))
			if err != nil {
				return err
			}
			defer ln.Close()
			fmt.Fprintf(a.stderr, "forwarding %s → %s:%s\n", ln.Addr(), args[0], args[1])
			go func() { <-ctx.Done(); ln.Close() }()
			for {
				c, err := ln.Accept()
				if err != nil {
					if ctx.Err() != nil {
						return nil
					}
					return err
				}
				go a.tunnel(ctx, c, path)
			}
		},
	}
	cmd.Flags().StringVar(&address, "address", "127.0.0.1", "local address to listen on")
	return cmd
}

// tunnel relays one local connection through its own stream.
func (a *app) tunnel(ctx context.Context, c net.Conn, path string) {
	defer c.Close()
	ws, err := a.c.Dial(ctx, path)
	if err != nil {
		fmt.Fprintln(a.stderr, "port-forward:", err)
		return
	}
	defer ws.CloseNow()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		pump(c, func(b []byte) bool { return wsjson.Write(ctx, ws, proto.StreamData{Data: b}) == nil })
		// Half-closed: the other way keeps going until the remote ends.
		_ = wsjson.Write(ctx, ws, proto.StreamData{EOF: true})
	}()
	for {
		var d proto.StreamData
		if err := wsjson.Read(ctx, ws, &d); err != nil {
			return
		}
		switch {
		case d.Error != "":
			fmt.Fprintln(a.stderr, "port-forward:", d.Error)
			return
		case d.ExitCode != nil:
			return
		case d.EOF:
			// The service is done sending; the client may still be.
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.CloseWrite()
			}
		default:
			if _, err := c.Write(d.Data); err != nil {
				return
			}
		}
	}
}

// pump reads r in chunks and hands each to fn (valid only until fn
// returns) until r ends or fn returns false.
func pump(r io.Reader, fn func([]byte) bool) {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 && !fn(buf[:n]) {
			return
		}
		if err != nil {
			return
		}
	}
}
