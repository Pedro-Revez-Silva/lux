package runner

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// directFake is a luxd with the direct upload routes and the S3 its
// presigned URLs point at. S3 refuses a body whose x-amz-checksum-sha256
// is not its own, and records every Authorization header it sees.
type directFake struct {
	mu        sync.Mutex
	luxd, s3  *httptest.Server
	upload    func(w http.ResponseWriter) bool                  // overrides /upload when it returns true
	complete  func(w http.ResponseWriter, r *http.Request) bool // overrides /complete when it returns true
	failPart  int                                               // S3 answers failCode (500 by default) to this part number
	failCode  int                                               //
	failTimes int                                               // that many times; 0 is always
	attempts  map[string]int                                    // part number or "single" → PUTs received
	at        map[string][]time.Time                            // part number or "single" → when each PUT arrived
	requests  []proto.BlobUploadRequest
	completes []proto.BlobCompleteRequest
	puts      []string // "<part number or single>:<bytes>"
	s3Auth    []string
	streamed  []string // blob ids PUT to luxd
	bodies    map[string][]byte
	signed    map[string]map[string]string // part number or "single" → headers presigned for it
}

func newDirectFake(t *testing.T) *directFake {
	delays := partRetryDelays
	partRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { partRetryDelays = delays })
	f := &directFake{bodies: map[string][]byte{}, attempts: map[string]int{}, at: map[string][]time.Time{}, signed: map[string]map[string]string{}}
	f.s3 = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.s3Auth = append(f.s3Auth, r.Header.Get("Authorization"))
		part := r.URL.Query().Get("partNumber")
		if part == "" {
			part = "single"
		}
		f.attempts[part]++
		f.at[part] = append(f.at[part], time.Now())
		if part == strconv.Itoa(f.failPart) && (f.failTimes == 0 || f.attempts[part] <= f.failTimes) {
			w.WriteHeader(cmp.Or(f.failCode, http.StatusInternalServerError))
			return
		}
		sum := sha256.Sum256(body)
		if r.Header.Get("X-Amz-Checksum-Sha256") != base64.StdEncoding.EncodeToString(sum[:]) ||
			r.Header.Get("Content-Length") != strconv.Itoa(len(body)) && r.ContentLength != int64(len(body)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// Every header presigned for this URL, unchanged, or the signature
		// does not hold.
		for k, v := range f.signed[part] {
			got := r.Header.Get(k)
			if k == "Content-Length" {
				got = strconv.FormatInt(r.ContentLength, 10)
			}
			if got != v {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, "SignatureDoesNotMatch: "+k)
				return
			}
		}
		f.puts = append(f.puts, part+":"+strconv.Itoa(len(body)))
		f.bodies[part] = body
		w.Header().Set("ETag", `"etag-`+part+`"`)
	}))
	t.Cleanup(f.s3.Close)
	f.luxd = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		id := strings.Split(strings.TrimPrefix(r.URL.Path, "/runner/v1/blobs/"), "/")[0]
		switch {
		case r.Method == http.MethodPut:
			_, _ = io.Copy(io.Discard, r.Body)
			f.mu.Lock()
			f.streamed = append(f.streamed, id)
			f.mu.Unlock()
		case strings.HasSuffix(r.URL.Path, "/upload"):
			var in proto.BlobUploadRequest
			_ = json.NewDecoder(r.Body).Decode(&in)
			f.mu.Lock()
			f.requests = append(f.requests, in)
			override := f.upload
			f.mu.Unlock()
			if override != nil && override(w) {
				return
			}
			// Signed like luxd's: the checksum, the length, the whole file's
			// sha256 metadata, and a header the runner knows nothing of.
			signed := func(sum string, n int64) map[string]string {
				return map[string]string{"X-Amz-Checksum-Sha256": sum, "Content-Length": strconv.FormatInt(n, 10),
					"X-Amz-Meta-Sha256": in.SHA256, "X-Amz-Foo": "bar"}
			}
			var out proto.BlobUploadResponse
			f.mu.Lock()
			if len(in.Parts) == 1 {
				out.Single = &proto.PresignedRequest{URL: f.s3.URL + "/b/" + id + "?X-Amz-Signature=x", Headers: signed(in.Parts[0], in.Size)}
				f.signed["single"] = out.Single.Headers
			} else {
				out.UploadID = "up-1"
				for i, sum := range in.Parts {
					p := proto.PresignedRequest{URL: f.s3.URL + "/b/" + id + "?uploadId=up-1&partNumber=" + strconv.Itoa(i+1),
						Headers: signed(sum, partLen(in.Size, i))}
					out.Parts = append(out.Parts, proto.PresignedPart{Number: int32(i + 1), PresignedRequest: p})
					f.signed[strconv.Itoa(i+1)] = p.Headers
				}
			}
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(out)
		case strings.HasSuffix(r.URL.Path, "/complete"):
			var in proto.BlobCompleteRequest
			_ = json.NewDecoder(r.Body).Decode(&in)
			f.mu.Lock()
			f.completes = append(f.completes, in)
			override := f.complete
			f.mu.Unlock()
			// luxd's answer: an abort names the upload id it drops.
			if in.Abort && in.UploadID == "" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"code":"bad_request","message":"abort needs an uploadId"}}`)
				return
			}
			if override != nil && override(w, r) {
				return
			}
			_, _ = io.WriteString(w, `{"uploaded":true}`)
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(f.luxd.Close)
	return f
}

func (f *directFake) api() *api { return newAPI(f.luxd.URL, "tok", "host") }

// blobFile writes a file of size bytes, each 4 KiB block distinct.
func blobFile(t *testing.T, size int64) (*os.File, []byte) {
	t.Helper()
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i*7 + i/4096)
	}
	path := filepath.Join(t.TempDir(), "blob.zst")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f, b
}

func b64(b []byte) string {
	s := sha256.Sum256(b)
	return base64.StdEncoding.EncodeToString(s[:])
}

func (f *directFake) checkNoTokenToS3(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.s3Auth) == 0 {
		t.Fatal("nothing reached S3")
	}
	for _, a := range f.s3Auth {
		if a != "" {
			t.Fatalf("S3 got Authorization %q", a)
		}
	}
}

// A blob over proto.BlobPartSize goes to S3 in parts, each with its own
// checksum, never with the host token; /upload carries the whole file's
// sha256 and each part's, /complete each part's ETag and checksum.
func TestUploadDirectMultipart(t *testing.T) {
	fk := newDirectFake(t)
	file, content := blobFile(t, proto.BlobPartSize+4097)
	if err := fk.api().uploadDirect(context.Background(), "blob1", file, int64(len(content))); err != nil {
		t.Fatal(err)
	}
	fk.checkNoTokenToS3(t)
	whole := sha256.Sum256(content)
	p1, p2 := content[:proto.BlobPartSize], content[proto.BlobPartSize:]
	req := fk.requests[0]
	if req.Size != int64(len(content)) || req.SHA256 != hex.EncodeToString(whole[:]) || len(req.Parts) != 2 ||
		req.Parts[0] != b64(p1) || req.Parts[1] != b64(p2) {
		t.Fatalf("/upload request %+v", req)
	}
	if !bytes.Equal(fk.bodies["1"], p1) || !bytes.Equal(fk.bodies["2"], p2) {
		t.Fatalf("S3 parts: %q", fk.puts)
	}
	want := twoPartDone(content)
	if len(fk.completes) != 1 || !equalComplete(fk.completes[0], want) {
		t.Fatalf("/complete %+v, want %+v", fk.completes, want)
	}
}

func equalComplete(a, b proto.BlobCompleteRequest) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return bytes.Equal(ja, jb)
}

// twoPartDone is the /complete of content's two-part upload through fk.
func twoPartDone(content []byte) proto.BlobCompleteRequest {
	p1, p2 := content[:proto.BlobPartSize], content[proto.BlobPartSize:]
	return proto.BlobCompleteRequest{UploadID: "up-1", Parts: []proto.UploadedPart{
		{Number: 1, ETag: `"etag-1"`, SHA256: b64(p1)}, {Number: 2, ETag: `"etag-2"`, SHA256: b64(p2)}}}
}

// A small blob is one PutObject with the whole file's checksum; an empty
// one too.
func TestUploadDirectSingle(t *testing.T) {
	for _, size := range []int64{0, 1000} {
		fk := newDirectFake(t)
		file, content := blobFile(t, size)
		if err := fk.api().uploadDirect(context.Background(), "blob1", file, size); err != nil {
			t.Fatal(err)
		}
		fk.checkNoTokenToS3(t)
		if len(fk.requests[0].Parts) != 1 || fk.requests[0].Parts[0] != b64(content) || !bytes.Equal(fk.bodies["single"], content) {
			t.Fatalf("size %d: request %+v puts %q", size, fk.requests[0], fk.puts)
		}
		if len(fk.completes) != 1 || !equalComplete(fk.completes[0], proto.BlobCompleteRequest{}) {
			t.Fatalf("size %d: /complete %+v", size, fk.completes)
		}
	}
}

// S3 failing part 2 for good is an upload error (the uploader retries the
// blob later), not luxd's "deleted": after its retries (a 5xx) or at once
// (a 4xx), the runner has luxd abort the upload id and never completes it.
func TestUploadDirectPartFailureIsAnError(t *testing.T) {
	for _, c := range []struct {
		status, attempts int
	}{{http.StatusInternalServerError, 3}, {http.StatusForbidden, 1}} {
		t.Run(strconv.Itoa(c.status), func(t *testing.T) {
			fk := newDirectFake(t)
			fk.failPart, fk.failCode = 2, c.status
			delays := []time.Duration{30 * time.Millisecond, 60 * time.Millisecond}
			partRetryDelays = delays
			file, content := blobFile(t, proto.BlobPartSize+10)
			err := fk.api().uploadDirect(context.Background(), "blob1", file, int64(len(content)))
			var he *httpError
			if err == nil || errors.As(err, &he) || errors.Is(err, errNoDirectUpload) {
				t.Fatalf("err %v, want a plain upload error", err)
			}
			if fk.attempts["2"] != c.attempts {
				t.Fatalf("part 2 attempted %d times, want %d", fk.attempts["2"], c.attempts)
			}
			// Each retry waits its delay after the attempt before.
			for i := 1; i < len(fk.at["2"]); i++ {
				if gap := fk.at["2"][i].Sub(fk.at["2"][i-1]); gap < delays[i-1] {
					t.Fatalf("attempt %d came %v after the one before, want ≥ %v", i+1, gap, delays[i-1])
				}
			}
			want := []proto.BlobCompleteRequest{{UploadID: "up-1", Abort: true}}
			if len(fk.completes) != 1 || !equalComplete(fk.completes[0], want[0]) {
				t.Fatalf("/complete %+v, want %+v", fk.completes, want)
			}
		})
	}
}

// A part S3 fails once is retried in place: the blob uploads in one pass,
// with one /upload.
func TestUploaderRetriesAPartInPlace(t *testing.T) {
	fk := newDirectFake(t)
	fk.failPart, fk.failTimes = 2, 1
	r, id := uploadRunner(t, fk, false)
	_, content := blobFile(t, proto.BlobPartSize+10)
	if err := os.WriteFile(r.blobPath("blob1"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	r.uploads.pass(context.Background())
	if !uploadDone(t, r, id) {
		t.Fatal("not done after one pass")
	}
	fk.mu.Lock()
	defer fk.mu.Unlock()
	if fk.attempts["2"] != 2 || len(fk.requests) != 1 || len(fk.completes) != 1 || fk.completes[0].Abort {
		t.Fatalf("part 2 attempts %d, /upload %d, /complete %+v", fk.attempts["2"], len(fk.requests), fk.completes)
	}
	if !bytes.Equal(fk.bodies["2"], content[proto.BlobPartSize:]) {
		t.Fatal("part 2's body")
	}
}

// failCompletes makes fk answer the first n multipart /complete requests
// (not aborts) with status and luxd's code; n < 0 is every one.
func (f *directFake) failCompletes(n, status int, code string) {
	f.complete = func(w http.ResponseWriter, _ *http.Request) bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		last := f.completes[len(f.completes)-1]
		if last.Abort || n == 0 {
			return false
		}
		n--
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":{"code":"`+code+`","message":"complete multipart upload failed"}}`)
		return true
	}
}

// A multipart /complete luxd fails once with 502 s3_unavailable is sent
// again, unchanged, in the same pass: one /upload, no abort.
func TestUploaderRetriesCompleteInPlace(t *testing.T) {
	fk := newDirectFake(t)
	fk.failCompletes(1, http.StatusBadGateway, "s3_unavailable")
	r, id := uploadRunner(t, fk, false)
	_, content := blobFile(t, proto.BlobPartSize+10)
	if err := os.WriteFile(r.blobPath("blob1"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	r.uploads.pass(context.Background())
	if !uploadDone(t, r, id) {
		t.Fatal("not done after one pass")
	}
	fk.mu.Lock()
	defer fk.mu.Unlock()
	want := twoPartDone(content)
	if len(fk.requests) != 1 || len(fk.completes) != 2 || !equalComplete(fk.completes[0], want) || !equalComplete(fk.completes[1], want) {
		t.Fatalf("/upload %d, /complete %+v, want twice %+v", len(fk.requests), fk.completes, want)
	}
}

// A multipart /complete that keeps failing (a 5xx, three attempts) or is
// refused (a 4xx, one attempt) is luxd's error, after which the runner has
// luxd abort that upload id.
func TestUploadDirectCompleteFailureAborts(t *testing.T) {
	for _, c := range []struct {
		status   int
		code     string
		attempts int
	}{{http.StatusBadGateway, "s3_unavailable", 3}, {http.StatusBadRequest, "invalid", 1}} {
		t.Run(strconv.Itoa(c.status), func(t *testing.T) {
			fk := newDirectFake(t)
			fk.failCompletes(-1, c.status, c.code)
			file, content := blobFile(t, proto.BlobPartSize+10)
			err := fk.api().uploadDirect(context.Background(), "blob1", file, int64(len(content)))
			var he *httpError
			if !errors.As(err, &he) || he.Status != c.status || he.Code != c.code {
				t.Fatalf("err %v, want luxd's %d %s", err, c.status, c.code)
			}
			var want []proto.BlobCompleteRequest
			for range c.attempts {
				want = append(want, twoPartDone(content))
			}
			want = append(want, proto.BlobCompleteRequest{UploadID: "up-1", Abort: true})
			ja, _ := json.Marshal(fk.completes)
			jb, _ := json.Marshal(want)
			if !bytes.Equal(ja, jb) {
				t.Fatalf("/complete %s, want %s", ja, jb)
			}
		})
	}
}

// uploadRunner is a Runner whose uploader talks to fk, with one reported
// record holding blob1.
func uploadRunner(t *testing.T, fk *directFake, published bool) (*Runner, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "snapshots"), 0o700); err != nil {
		t.Fatal(err)
	}
	r := &Runner{cfg: Config{DataDir: dir}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r.api = fk.api()
	r.uploads = newUploader(r)
	blob := r.blobPath("blob1")
	if err := os.WriteFile(blob, []byte("blob bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := &snapshotRecord{RunID: "run1", Epoch: 1, Reported: true, Uploads: []pendingUpload{{BlobID: "blob1", Path: blob, Size: 10}}}
	if published {
		rec.Published = &proto.ArtifactPublished{ID: "art_aaaaaaaaaaaaaaaa"}
	}
	if err := r.saveSnapshotRecord("rec1", rec); err != nil {
		t.Fatal(err)
	}
	return r, "rec1"
}

func uploadDone(t *testing.T, r *Runner, id string) bool {
	t.Helper()
	rec, err := r.readRecord(id)
	if err != nil {
		t.Fatal(err)
	}
	return rec.Uploads[0].Done
}

// The uploader's answer to each /upload outcome: presigned URLs ⇒ direct
// to S3; a 404 with no luxd error body, or 405 (a luxd without the route)
// ⇒ the streaming PUT; luxd's not_found (the blob is gone) ⇒ done, no PUT;
// a 500 ⇒ not done, retried later.
func TestUploaderDirectAndFallback(t *testing.T) {
	for _, c := range []struct {
		name            string
		status          int
		body            string
		published       bool
		wantDone        bool
		wantS3, wantPut bool
	}{
		{name: "direct", wantDone: true, wantS3: true},
		{name: "published artifact direct", published: true, wantDone: true, wantS3: true},
		{name: "404 without body", status: http.StatusNotFound, body: "404 page not found\n", wantDone: true, wantPut: true},
		{name: "405", status: http.StatusMethodNotAllowed, body: "Method Not Allowed\n", wantDone: true, wantPut: true},
		{name: "lux not_found", status: http.StatusNotFound, body: `{"error":{"code":"not_found","message":"no blob blob1 from this host"}}`, wantDone: true},
		{name: "lux gone", status: http.StatusGone, body: `{"error":{"code":"gone","message":"deleted"}}`, wantDone: true},
		{name: "500", status: http.StatusInternalServerError, body: `{"error":{"code":"internal","message":"internal error"}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			fk := newDirectFake(t)
			if c.status != 0 {
				fk.upload = func(w http.ResponseWriter) bool {
					w.WriteHeader(c.status)
					_, _ = io.WriteString(w, c.body)
					return true
				}
			}
			r, id := uploadRunner(t, fk, c.published)
			r.uploads.pass(context.Background())
			if got := uploadDone(t, r, id); got != c.wantDone {
				t.Errorf("done %v, want %v", got, c.wantDone)
			}
			fk.mu.Lock()
			defer fk.mu.Unlock()
			if got := len(fk.puts) == 1 && len(fk.completes) == 1; got != c.wantS3 {
				t.Errorf("S3 puts %q completes %d, want direct: %v", fk.puts, len(fk.completes), c.wantS3)
			}
			if c.wantS3 && string(fk.bodies["single"]) != "blob bytes" {
				t.Errorf("S3 body %q", fk.bodies["single"])
			}
			if got := len(fk.streamed) == 1; got != c.wantPut {
				t.Errorf("streamed %v, want PUT through luxd: %v", fk.streamed, c.wantPut)
			}
			for _, a := range fk.s3Auth {
				if a != "" {
					t.Errorf("S3 got Authorization %q", a)
				}
			}
		})
	}
}

// A /complete answered by the mux's own 404 (no luxd error code) recorded
// nothing: the blob is not done, and a later pass completes it.
func TestUploaderCompletePlain404IsRetried(t *testing.T) {
	fk := newDirectFake(t)
	r, id := uploadRunner(t, fk, false)
	fk.complete = func(w http.ResponseWriter, r *http.Request) bool {
		http.NotFound(w, r)
		return true
	}
	r.uploads.pass(context.Background())
	if uploadDone(t, r, id) {
		t.Fatal("done though /complete answered a plain 404")
	}
	fk.mu.Lock()
	fk.complete = nil
	fk.mu.Unlock()
	r.uploads.pass(context.Background())
	if !uploadDone(t, r, id) || len(fk.completes) != 2 {
		t.Fatalf("done %v after %d /complete requests", uploadDone(t, r, id), len(fk.completes))
	}
}

// The fallback to the streaming PUT is logged once per process, not once
// per blob.
func TestUploaderFallbackWarnsOnce(t *testing.T) {
	fallbackWarning = sync.Once{}
	t.Cleanup(func() { fallbackWarning = sync.Once{} })
	fk := newDirectFake(t)
	fk.upload = func(w http.ResponseWriter) bool {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "404 page not found\n")
		return true
	}
	r, _ := uploadRunner(t, fk, false)
	var logs bytes.Buffer
	r.log = slog.New(slog.NewTextHandler(&logs, nil))
	up := pendingUpload{BlobID: "blob1", Path: r.blobPath("blob1"), Size: 10}
	for range 2 {
		if err := r.uploads.upload(context.Background(), up); err != nil {
			t.Fatal(err)
		}
	}
	if len(fk.streamed) != 2 {
		t.Fatalf("streamed %v, want 2 PUTs through luxd", fk.streamed)
	}
	if n := strings.Count(logs.String(), "no direct upload route"); n != 1 {
		t.Fatalf("%d fallback warnings, want 1:\n%s", n, logs.String())
	}
}

// partLen over the same sizes as proto's BlobParts table: every part but
// the last is BlobPartSize, the last of an exact multiple is BlobPartSize
// too (never 0), and the lengths add up to the size.
func TestPartLen(t *testing.T) {
	const p = proto.BlobPartSize
	for _, c := range []struct {
		size int64
		last int64
	}{
		{0, 0}, {1, 1}, {p - 1, p - 1}, {p, p}, {p + 1, 1}, {2 * p, p}, {2*p + 1, 1},
		{7_300_000_000, 7_300_000_000 - 108*p}, {114*p + 1, 1}, {115 * p, p},
	} {
		n := proto.BlobParts(c.size)
		var total int64
		for i := range n {
			l := partLen(c.size, i)
			if i < n-1 && l != p {
				t.Errorf("size %d: part %d is %d bytes, want %d", c.size, i+1, l, p)
			}
			total += l
		}
		if got := partLen(c.size, n-1); got != c.last {
			t.Errorf("size %d: last part (%d of %d) is %d bytes, want %d", c.size, n, n, got, c.last)
		}
		if total != c.size {
			t.Errorf("size %d: parts add up to %d", c.size, total)
		}
	}
}

// A retry after a failed part starts a fresh /upload.
func TestUploaderRetryStartsAFreshUpload(t *testing.T) {
	fk := newDirectFake(t)
	r, id := uploadRunner(t, fk, false)
	first := true
	fk.upload = func(w http.ResponseWriter) bool {
		fk.mu.Lock()
		defer fk.mu.Unlock()
		if !first {
			return false
		}
		first = false
		w.WriteHeader(http.StatusServiceUnavailable)
		return true
	}
	r.uploads.pass(context.Background())
	if uploadDone(t, r, id) {
		t.Fatal("done after a failed /upload")
	}
	r.uploads.pass(context.Background())
	if !uploadDone(t, r, id) || len(fk.requests) != 2 {
		t.Fatalf("done %v after %d /upload requests", uploadDone(t, r, id), len(fk.requests))
	}
}
