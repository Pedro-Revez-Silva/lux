package shim

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

const publishUsage = "usage: lux-shim publish FILE [--name NAME] [--description TEXT] [--content-type TYPE]"

// Publish is `lux-shim publish`'s main: it sends FILE's bytes to the shim,
// which keeps a copy as an artifact of the Run, and prints the shim's
// answer as one JSON line.
func Publish(args []string) int {
	file, req, err := parsePublishArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lux-shim publish:", err)
		fmt.Fprintln(os.Stderr, publishUsage)
		return 2
	}
	rep, err := publishFile(proto.ShimPublishSocket, file, req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lux-shim publish:", err)
		return 1
	}
	if err := json.NewEncoder(os.Stdout).Encode(rep); err != nil {
		return 1
	}
	return 0
}

func parsePublishArgs(args []string) (string, proto.PublishRequest, error) {
	var req proto.PublishRequest
	var file string
	var nameSet bool
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			if file != "" || i+2 != len(args) {
				return "", req, errors.New("one FILE")
			}
			file = args[i+1]
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			if file != "" {
				return "", req, errors.New("one FILE")
			}
			file = a
			continue
		}
		key, val, inline := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !inline {
			if i+1 >= len(args) {
				return "", req, fmt.Errorf("%s needs a value", a)
			}
			i++
			val = args[i]
		}
		switch key {
		case "name":
			req.Name, nameSet = val, true
		case "description":
			req.Description = val
		case "content-type":
			req.ContentType = val
		default:
			return "", req, fmt.Errorf("unknown flag %s", a)
		}
	}
	if file == "" {
		return "", req, errors.New("no FILE")
	}
	if !nameSet {
		req.Name = filepath.Base(file)
	}
	return file, req, nil
}

// publishFile opens file as the caller and streams it to the shim's
// publish socket.
func publishFile(socket, file string, req proto.PublishRequest) (proto.PublishReply, error) {
	f, err := os.Open(file)
	if err != nil {
		return proto.PublishReply{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return proto.PublishReply{}, err
	}
	if !fi.Mode().IsRegular() {
		return proto.PublishReply{}, fmt.Errorf("%s: not a regular file", file)
	}
	req.Size = fi.Size()
	c, err := net.Dial("unix", socket)
	if err != nil {
		return proto.PublishReply{}, fmt.Errorf("the shim's publish socket: %w (lux-shim publish works only inside a lux Run)", err)
	}
	defer c.Close()
	return sendPublish(c.(*net.UnixConn), f, req)
}

// sendPublish writes the request and the body, then reads the answer. A
// write that fails (the shim refused the request at its header and closed)
// still reads the answer the shim wrote first.
func sendPublish(c *net.UnixConn, body io.Reader, req proto.PublishRequest) (proto.PublishReply, error) {
	hdr, _ := json.Marshal(req)
	werr := func() error {
		if _, err := c.Write(append(hdr, '\n')); err != nil {
			return err
		}
		n, err := io.CopyN(c, body, req.Size)
		if err != nil {
			// The shim sees the end and refuses a short body.
			_ = c.CloseWrite()
			return fmt.Errorf("read %d of %d bytes: %w", n, req.Size, err)
		}
		return c.CloseWrite()
	}()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Minute))
	line, rerr := bufio.NewReader(c).ReadBytes('\n')
	var rep proto.PublishReply
	if rerr != nil || json.Unmarshal(line, &rep) != nil {
		if werr != nil {
			return rep, werr
		}
		return rep, errors.New("no answer from the shim")
	}
	if rep.Error != "" {
		return rep, errors.New(rep.Error)
	}
	if werr != nil {
		return rep, werr
	}
	return rep, nil
}
