package shim

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
)

// publisher stages what `lux-shim publish` sends (proto.PublishRequest)
// in base/rel and records it with emit. It reads only the bytes on the
// connection: nothing the client names is opened. Files are opened
// through an os.Root on base (the runtime volume): a root workload's link
// in place of the staging directory cannot lead them off the volume.
type publisher struct {
	base string
	// rel is the staging directory relative to base, as records name files.
	rel  string
	max  int64
	emit func(proto.StagedArtifact) error
}

// maxPublishHeader bounds the request line.
const maxPublishHeader = 64 << 10

// accept serves each connection on ln until it is closed.
func (p *publisher) accept(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go p.serve(c)
	}
}

// serve answers one connection with one proto.PublishReply line.
func (p *publisher) serve(c net.Conn) {
	defer c.Close()
	rep, err := p.publish(c)
	if err != nil {
		rep = proto.PublishReply{Error: err.Error()}
	}
	_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_ = json.NewEncoder(c).Encode(rep)
}

func (p *publisher) publish(c net.Conn) (proto.PublishReply, error) {
	br := bufio.NewReaderSize(c, maxPublishHeader)
	line, err := br.ReadSlice('\n')
	if err != nil {
		return proto.PublishReply{}, fmt.Errorf("bad request: %w", err)
	}
	var req proto.PublishRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return proto.PublishReply{}, fmt.Errorf("bad request: %w", err)
	}
	if err := checkRequest(req, p.max); err != nil {
		return proto.PublishReply{}, err
	}
	root, err := os.OpenRoot(p.base)
	if err != nil {
		return proto.PublishReply{}, fmt.Errorf("staging: %w", err)
	}
	defer root.Close()
	id := ids.New(ids.Artifact)
	tmp := path.Join(p.rel, "."+id+".tmp")
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return proto.PublishReply{}, fmt.Errorf("staging: %w", err)
	}
	staged := false
	defer func() {
		if !staged {
			f.Close()
			root.Remove(tmp)
		}
	}()
	h := sha256.New()
	head := &headWriter{}
	// Exactly the declared size, then the client's half-close: fewer bytes
	// is a short read, more is over what was declared. Neither is staged.
	n, err := io.CopyN(io.MultiWriter(f, h, head), br, req.Size)
	if err != nil {
		return proto.PublishReply{}, fmt.Errorf("short read: %d of %d bytes", n, req.Size)
	}
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := br.ReadByte(); !errors.Is(err, io.EOF) {
		if err == nil {
			return proto.PublishReply{}, fmt.Errorf("more than the declared %d bytes", req.Size)
		}
		return proto.PublishReply{}, fmt.Errorf("no end after %d bytes: %w", req.Size, err)
	}
	if err := f.Sync(); err != nil {
		return proto.PublishReply{}, fmt.Errorf("staging: %w", err)
	}
	if err := f.Close(); err != nil {
		return proto.PublishReply{}, fmt.Errorf("staging: %w", err)
	}
	final := path.Join(p.rel, id)
	if err := root.Rename(tmp, final); err != nil {
		return proto.PublishReply{}, fmt.Errorf("staging: %w", err)
	}
	staged = true
	ctype := req.ContentType
	if ctype == "" {
		ctype = proto.DetectContentType(req.Name, head.b)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	a := proto.StagedArtifact{ID: id, Name: req.Name, Description: req.Description, ContentType: ctype,
		Size: n, SHA256: sum, File: final}
	if err := p.emit(a); err != nil {
		root.Remove(final)
		return proto.PublishReply{}, err
	}
	return proto.PublishReply{ID: id, Name: req.Name, Size: n, SHA256: sum}, nil
}

func checkRequest(req proto.PublishRequest, max int64) error {
	if err := proto.ValidPublishFields(req.Name, req.ContentType, req.Description); err != nil {
		return err
	}
	if req.Size < 0 {
		return errors.New("negative size")
	}
	if req.Size > max {
		return fmt.Errorf("%d bytes: over the %d-byte limit", req.Size, max)
	}
	return nil
}

// headWriter keeps the first 512 bytes written (what sniffing reads).
type headWriter struct{ b []byte }

func (w *headWriter) Write(p []byte) (int, error) {
	if room := 512 - len(w.b); room > 0 {
		w.b = append(w.b, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// prepareStaging makes the staging directory root's and empty: nothing
// staged by an earlier placement is still wanted (the runner handled it
// before this one could start), and the workload may not write there.
func prepareStaging(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	return os.Chown(dir, 0, 0)
}

// listenPublish opens the publish socket for the workload user alone.
func (s *Shim) listenPublish() (net.Listener, error) {
	os.Remove(proto.ShimPublishSocket)
	ln, err := net.Listen("unix", proto.ShimPublishSocket)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(proto.ShimPublishSocket, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	if err := os.Chown(proto.ShimPublishSocket, s.user.uid, s.user.gid); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// startPublish serves `lux-shim publish` from now until the shim ends.
func (s *Shim) startPublish() error {
	if s.cfg.ArtifactsDir == "" {
		return nil
	}
	rel, err := filepath.Rel(proto.ShimRunDir, s.cfg.ArtifactsDir)
	if err != nil || !filepath.IsLocal(rel) {
		return fmt.Errorf("artifacts staging %s is not on the runtime volume", s.cfg.ArtifactsDir)
	}
	if err := prepareStaging(s.cfg.ArtifactsDir); err != nil {
		return fmt.Errorf("artifacts staging: %w", err)
	}
	ln, err := s.listenPublish()
	if err != nil {
		return fmt.Errorf("publish socket: %w", err)
	}
	p := &publisher{base: proto.ShimRunDir, rel: filepath.ToSlash(rel), max: proto.MaxArtifactBytes, emit: func(a proto.StagedArtifact) error {
		return s.out.TryEvent(proto.EvArtifact, a)
	}}
	go p.accept(ln)
	return nil
}
