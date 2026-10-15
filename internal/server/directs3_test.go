package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/marcioapm/lux/internal/blob"
	"github.com/marcioapm/lux/internal/proto"
)

// directS3 is an S3 endpoint for direct uploads: PutObject, UploadPart,
// the multipart calls, HeadObject and DeleteObject on bucket b. Like S3,
// it refuses a body whose x-amz-checksum-sha256 does not match, a
// presigned PUT without that header (luxd always signs it in), and a
// CompleteMultipartUpload whose parts are not uploaded ones, in ascending
// order, with their ETags and checksums. It keeps object sizes only, and
// logs every call as "METHOD op key".
type directS3 struct {
	mu      sync.Mutex
	calls   []string
	objects map[string]int64                // key → size
	parts   map[string]map[int32]uploadedS3 // upload id → part number → part
	deleted []string
	next    int
	onHead  func() // called after a HeadObject's lookup, before its answer
	// completeErr, when set, is CompleteMultipartUpload's answer: a status
	// and an S3 error code.
	completeErr *s3Err
}

type uploadedS3 struct {
	size           int64
	etag, checksum string
}

type s3Err struct {
	status int
	code   string
}

func useDirectS3(t *testing.T, s *Server) *directS3 {
	t.Helper()
	f := &directS3{objects: map[string]int64{}, parts: map[string]map[int32]uploadedS3{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	bs, err := blob.New(context.Background(), blob.Config{Endpoint: srv.URL, Bucket: "b", AccessKey: "k", SecretKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	s.blobs = bs
	return f
}

func (f *directS3) serve(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/b/")
	q := r.URL.Query()
	body, _ := io.ReadAll(r.Body)
	if r.Method == http.MethodHead {
		// Looked up first, then onHead: a delete there lands after S3
		// answered, before luxd records the upload.
		f.mu.Lock()
		f.calls = append(f.calls, "HEAD HeadObject "+key)
		size, ok := f.objects[key]
		hook := f.onHead
		f.mu.Unlock()
		if hook != nil {
			hook()
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	op := ""
	switch {
	case r.Method == http.MethodPut && q.Has("partNumber"):
		op = "UploadPart"
	case r.Method == http.MethodPut:
		op = "PutObject"
	case r.Method == http.MethodPost && q.Has("uploads"):
		op = "CreateMultipartUpload"
	case r.Method == http.MethodPost && q.Has("uploadId"):
		op = "CompleteMultipartUpload"
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		op = "AbortMultipartUpload"
	case r.Method == http.MethodDelete:
		op = "DeleteObject"
	case r.Method == http.MethodHead:
		op = "HeadObject"
	}
	f.calls = append(f.calls, r.Method+" "+op+" "+key)
	checksum := r.Header.Get("X-Amz-Checksum-Sha256")
	if op == "PutObject" || op == "UploadPart" {
		// Checked when sent, as S3 does. A presigned URL has it signed in,
		// so S3 refuses the request without it; luxd's own streaming
		// uploads (the SDK) may carry another checksum instead.
		sum := sha256.Sum256(body)
		if checksum == "" && q.Has("X-Amz-Signature") || checksum != "" && checksum != base64.StdEncoding.EncodeToString(sum[:]) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `<Error><Code>BadDigest</Code></Error>`)
			return
		}
	}
	switch op {
	case "PutObject":
		f.objects[key] = int64(len(body))
		w.Header().Set("ETag", `"put"`)
	case "UploadPart":
		up, ok := f.parts[q.Get("uploadId")]
		n, err := strconv.ParseInt(q.Get("partNumber"), 10, 32)
		if !ok || err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		etag := `"part-` + q.Get("partNumber") + `-` + strconv.Itoa(len(body)) + `"`
		up[int32(n)] = uploadedS3{size: int64(len(body)), etag: etag, checksum: checksum}
		w.Header().Set("ETag", etag)
	case "CreateMultipartUpload":
		f.next++
		id := fmt.Sprintf("up-%d", f.next)
		f.parts[id] = map[int32]uploadedS3{}
		fmt.Fprintf(w, `<InitiateMultipartUploadResult><Bucket>b</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, key, id)
	case "CompleteMultipartUpload":
		if e := f.completeErr; e != nil {
			w.WriteHeader(e.status)
			fmt.Fprintf(w, `<Error><Code>%s</Code></Error>`, e.code)
			return
		}
		up, ok := f.parts[q.Get("uploadId")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `<Error><Code>NoSuchUpload</Code></Error>`)
			return
		}
		var done struct {
			Parts []struct {
				PartNumber     int32
				ETag           string
				ChecksumSHA256 string
			} `xml:"Part"`
		}
		if err := xml.Unmarshal(body, &done); err != nil || len(done.Parts) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `<Error><Code>MalformedXML</Code></Error>`)
			return
		}
		var n int64
		for i, p := range done.Parts {
			got, ok := up[p.PartNumber]
			if !ok || p.ETag != got.etag || p.ChecksumSHA256 != got.checksum {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `<Error><Code>InvalidPart</Code></Error>`)
				return
			}
			if i > 0 && p.PartNumber <= done.Parts[i-1].PartNumber {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `<Error><Code>InvalidPartOrder</Code></Error>`)
				return
			}
			n += got.size
		}
		f.objects[key] = n
		delete(f.parts, q.Get("uploadId"))
		fmt.Fprintf(w, `<CompleteMultipartUploadResult><Bucket>b</Bucket><Key>%s</Key><ETag>"mp"</ETag></CompleteMultipartUploadResult>`, key)
	case "AbortMultipartUpload":
		delete(f.parts, q.Get("uploadId"))
		w.WriteHeader(http.StatusNoContent)
	case "DeleteObject":
		delete(f.objects, key)
		f.deleted = append(f.deleted, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

func (f *directS3) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *directS3) Deleted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.deleted)
}

func (f *directS3) setObject(key string, size int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = size
}

// runnerPost POSTs in as JSON to a runner blob route as host.
func runnerPost(t *testing.T, s *Server, host, id, op string, in any) (int, string) {
	t.Helper()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/runner/v1/blobs/"+id+"/"+op+"?host="+host, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer host-secret")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func b64sum(b []byte) string {
	sum := sha256.Sum256(b)
	return base64.StdEncoding.EncodeToString(sum[:])
}

// uploadRequest describes content as the runner does: the whole file's
// hex sha256 and each proto.BlobPartSize part's base64 sha256.
func uploadRequest(content []byte) proto.BlobUploadRequest {
	sum := sha256.Sum256(content)
	in := proto.BlobUploadRequest{Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:])}
	for i := 0; i < proto.BlobParts(int64(len(content))); i++ {
		in.Parts = append(in.Parts, b64sum(content[i*proto.BlobPartSize:min(len(content), (i+1)*proto.BlobPartSize)]))
	}
	return in
}

// putPresigned PUTs body to a presigned request with exactly its headers,
// as the runner does. Returns the status and ETag. A signed Content-Length
// other than the body's fails the test: S3 would refuse the signature.
func putPresigned(t *testing.T, p proto.PresignedRequest, body []byte) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, p.URL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range p.Headers {
		if k == "Content-Length" {
			if v != strconv.Itoa(len(body)) {
				t.Fatalf("presigned Content-Length %s, body %d bytes", v, len(body))
			}
			continue // the body's; Go sets it
		}
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, resp.Header.Get("ETag")
}

// startUpload asks ha's /upload for blob id's URLs, failing the test on
// anything but 200.
func startUpload(t *testing.T, s *Server, id string, content []byte) proto.BlobUploadResponse {
	t.Helper()
	code, raw := runnerPost(t, s, "ha", id, "upload", uploadRequest(content))
	if code != http.StatusOK {
		t.Fatalf("upload: %d %s", code, raw)
	}
	var out proto.BlobUploadResponse
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// putSingle starts ha's upload of a single-part blob id and PUTs content
// to its presigned URL, without /complete.
func putSingle(t *testing.T, s *Server, id string, content []byte) {
	t.Helper()
	out := startUpload(t, s, id, content)
	if code, _ := putPresigned(t, *out.Single, content); code != http.StatusOK {
		t.Fatalf("PUT: %d", code)
	}
}

// hostArtifact puts bA-art back on its host with content's sha256 and size.
func hostArtifact(t *testing.T, s *Server, ctx context.Context, content []byte) {
	t.Helper()
	execSQL(t, s, ctx, `UPDATE blobs SET location = 'host', s3_key = NULL, sha256 = $1, size = $2 WHERE id = 'bA-art'`, sha(string(content)), len(content))
}

// directUpload uploads content to blob id from host through /upload, the
// presigned PUTs and /complete. Returns /complete's status and body.
func directUpload(t *testing.T, s *Server, host, id string, content []byte) (int, string) {
	t.Helper()
	code, raw := runnerPost(t, s, host, id, "upload", uploadRequest(content))
	if code != http.StatusOK {
		return code, raw
	}
	var out proto.BlobUploadResponse
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	if out.Uploaded {
		return code, raw
	}
	var done proto.BlobCompleteRequest
	if out.Single != nil {
		if code, _ := putPresigned(t, *out.Single, content); code != http.StatusOK {
			t.Fatalf("PUT: %d", code)
		}
	} else {
		done.UploadID = out.UploadID
		for i, p := range out.Parts {
			part := content[i*proto.BlobPartSize : min(len(content), (i+1)*proto.BlobPartSize)]
			code, etag := putPresigned(t, p.PresignedRequest, part)
			if code != http.StatusOK {
				t.Fatalf("PUT part %d: %d", p.Number, code)
			}
			done.Parts = append(done.Parts, proto.UploadedPart{Number: p.Number, ETag: etag, SHA256: b64sum(part)})
		}
	}
	return runnerPost(t, s, host, id, "complete", done)
}

// setBlob gives blob id the size and sha256 of content.
func setBlob(t *testing.T, s *Server, id string, content []byte) {
	t.Helper()
	execSQL(t, s, context.Background(), `UPDATE blobs SET size = $2, sha256 = $3 WHERE id = $1`, id, len(content), sha(string(content)))
}

// uploadState is what an upload records: the blob rows, the snapshot's and
// placement's uploaded marks, and artifact.published events.
func uploadState(t *testing.T, s *Server) string {
	t.Helper()
	var snap bool
	var placement, uploadedAt int
	systemScan(t, s, `SELECT uploaded FROM snapshots WHERE id = 'snapA'`, nil, &snap)
	systemScan(t, s, `SELECT count(*) FROM placements WHERE run_id = 'ra' AND uploaded_at IS NOT NULL`, nil, &placement)
	systemScan(t, s, `SELECT count(*) FROM blobs WHERE run_id = 'ra' AND uploaded_at IS NOT NULL AND s3_key IS NOT NULL`, nil, &uploadedAt)
	evs := publishedEvents(t, s, "ra")
	paths := []string{}
	for _, e := range evs {
		paths = append(paths, fmt.Sprint(e["path"]))
	}
	return fmt.Sprintf("blobs %v\nsnapshot uploaded %v; placements uploaded %d; blobs with uploaded_at %d; events %v",
		recordsOf(t, s, "ra").Blobs, snap, placement, uploadedAt, paths)
}

// Uploading every blob of ra's snapshot straight to S3 (the volume in
// several parts) records exactly what the streaming PUT records: blobs in
// s3 with their keys, the snapshot and placement uploaded, the artifact's
// event.
func TestDirectUploadRecordsLikeStreamingUpload(t *testing.T) {
	small := []byte("artifact bytes")
	out := []byte("output bytes")
	vol := bytes.Repeat([]byte("v"), proto.BlobPartSize+5)
	contents := map[string][]byte{"bA-vol": vol, "bA-out": out, "bA-art": small}
	ids := []string{"bA-vol", "bA-out", "bA-art"}

	old, _ := reportFixture(t)
	useDirectS3(t, old)
	direct, _ := reportFixture(t)
	f := useDirectS3(t, direct)
	for _, s := range []*Server{old, direct} {
		for _, id := range ids {
			setBlob(t, s, id, contents[id])
		}
	}
	before := uploadState(t, direct)
	for _, id := range ids {
		if code := uploadBlob(t, old, "ha", id, string(contents[id])); code != http.StatusOK {
			t.Fatalf("streaming upload %s: %d", id, code)
		}
		if code, body := directUpload(t, direct, "ha", id, contents[id]); code != http.StatusOK || !strings.Contains(body, `"uploaded":true`) {
			t.Fatalf("direct upload %s: %d %s", id, code, body)
		}
	}
	got, want := uploadState(t, direct), uploadState(t, old)
	if got != want {
		t.Fatalf("direct upload records\n%s\nstreaming records\n%s", got, want)
	}
	if got == before || !strings.Contains(got, "snapshot uploaded true; placements uploaded 1; blobs with uploaded_at 3; events [/out/a.txt]") {
		t.Fatalf("not recorded as uploaded: %s", got)
	}
	var keys []string
	for _, id := range ids {
		keys = append(keys, blob.Key("t1", "ra", id))
	}
	f.mu.Lock()
	for i, k := range keys {
		if size, ok := f.objects[k]; !ok || size != int64(len(contents[ids[i]])) {
			t.Errorf("object %s: %d %v", k, size, ok)
		}
	}
	f.mu.Unlock()
	calls := f.Calls()
	if !slices.Contains(calls, "POST CreateMultipartUpload "+keys[0]) || !slices.Contains(calls, "POST CompleteMultipartUpload "+keys[0]) ||
		!slices.Contains(calls, "PUT PutObject "+keys[2]) {
		t.Fatalf("S3 calls %q", calls)
	}
	// Again: already in S3, no URLs.
	code, body := runnerPost(t, direct, "ha", "bA-art", "upload", uploadRequest(small))
	if code != http.StatusOK || strings.TrimSpace(body) != `{"uploaded":true}` {
		t.Fatalf("upload of a blob in s3: %d %s", code, body)
	}
}

// /upload refuses, without calling S3: another host's blob (404), a
// sha256 or size other than the reported one (400), a part count that
// does not match the size (400).
func TestDirectUploadRefusals(t *testing.T) {
	s, _ := reportFixture(t)
	f := useDirectS3(t, s)
	content := []byte("artifact bytes")
	setBlob(t, s, "bA-art", content)
	wrongSum := uploadRequest([]byte("other bytes!!!"))
	wrongSize := uploadRequest(content)
	wrongSize.Size++
	noParts := uploadRequest(content)
	noParts.Parts = nil
	twoParts := uploadRequest(content)
	twoParts.Parts = append(twoParts.Parts, twoParts.Parts[0])
	badSum := uploadRequest(content)
	badSum.SHA256 = "zz" + badSum.SHA256[2:]
	otherPart := uploadRequest(content)
	otherPart.Parts = []string{b64sum([]byte("other bytes!!!"))}
	for _, c := range []struct {
		name, host string
		in         proto.BlobUploadRequest
		code       int
		errCode    string
	}{
		{"another host", "hb", uploadRequest(content), http.StatusNotFound, "not_found"},
		{"wrong sha256", "ha", wrongSum, http.StatusBadRequest, "checksum_mismatch"},
		{"malformed sha256", "ha", badSum, http.StatusBadRequest, "bad_request"},
		{"single part with another checksum", "ha", otherPart, http.StatusBadRequest, "checksum_mismatch"},
		{"wrong size", "ha", wrongSize, http.StatusBadRequest, "size_mismatch"},
		{"no parts", "ha", noParts, http.StatusBadRequest, "bad_request"},
		{"two parts", "ha", twoParts, http.StatusBadRequest, "bad_request"},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, body := runnerPost(t, s, c.host, "bA-art", "upload", c.in)
			if code != c.code || !strings.Contains(body, `"code":"`+c.errCode+`"`) {
				t.Fatalf("%d %s, want %d %s", code, body, c.code, c.errCode)
			}
		})
	}
	if calls := f.Calls(); len(calls) != 0 {
		t.Fatalf("S3 called: %q", calls)
	}
	// More parts than S3 allows, on a row without a size (no size check):
	// refused before any presigning.
	execSQL(t, s, context.Background(), `UPDATE blobs SET size = 0 WHERE id = 'bA-art'`)
	tooMany := uploadRequest(content)
	tooMany.Size = 10001 * proto.BlobPartSize
	tooMany.Parts = slices.Repeat([]string{b64sum(nil)}, 10001)
	if code, body := runnerPost(t, s, "ha", "bA-art", "upload", tooMany); code != http.StatusBadRequest || !strings.Contains(body, "at most 10000") {
		t.Fatalf("10001 parts: %d %s", code, body)
	}
	if calls := f.Calls(); len(calls) != 0 {
		t.Fatalf("S3 called: %q", calls)
	}
	setBlob(t, s, "bA-art", content)
	// A body other than the one presigned is refused by S3.
	out := startUpload(t, s, "bA-art", content)
	if code, _ := putPresigned(t, *out.Single, []byte("other bytes!!!")); code == http.StatusOK {
		t.Fatal("S3 accepted another body")
	}
	if loc := blobLocations(t, s, "ra")["bA-art"]; loc != "host" {
		t.Fatalf("bA-art: %s", loc)
	}
}

// A row with no sha256 (and size 0) is refused, as by the streaming PUT:
// the runner's own digest is never taken in its place.
func TestDirectUploadRefusesRowWithoutSHA256(t *testing.T) {
	s, _ := reportFixture(t)
	f := useDirectS3(t, s)
	execSQL(t, s, context.Background(), `UPDATE blobs SET size = 0, sha256 = '' WHERE id = 'bA-art'`)
	code, body := runnerPost(t, s, "ha", "bA-art", "upload", uploadRequest([]byte("artifact bytes")))
	if code != http.StatusBadRequest || !strings.Contains(body, `"code":"checksum_mismatch"`) {
		t.Fatalf("upload: %d %s, want 400 checksum_mismatch", code, body)
	}
	if loc := blobLocations(t, s, "ra")["bA-art"]; loc != "host" {
		t.Fatalf("bA-art: %s, want host", loc)
	}
	if calls := f.Calls(); len(calls) != 0 {
		t.Fatalf("S3 called: %q", calls)
	}
}

// /complete for an object whose length is not the blob's deletes it and
// answers 400; for no object at all, 409. The blob stays on its host.
func TestDirectUploadCompleteChecksSize(t *testing.T) {
	s, _ := reportFixture(t)
	f := useDirectS3(t, s)
	content := []byte("artifact bytes")
	setBlob(t, s, "bA-art", content)
	key := blob.Key("t1", "ra", "bA-art")
	if code, body := runnerPost(t, s, "ha", "bA-art", "complete", proto.BlobCompleteRequest{}); code != http.StatusConflict {
		t.Fatalf("complete with no object: %d %s, want 409", code, body)
	}
	f.setObject(key, int64(len(content))+1)
	code, body := runnerPost(t, s, "ha", "bA-art", "complete", proto.BlobCompleteRequest{})
	if code != http.StatusBadRequest || !strings.Contains(body, "size_mismatch") {
		t.Fatalf("complete: %d %s, want 400 size_mismatch", code, body)
	}
	if got := f.Deleted(); !slices.Equal(got, []string{key}) {
		t.Fatalf("deletes %v, want %s", got, key)
	}
	if loc := blobLocations(t, s, "ra")["bA-art"]; loc != "host" {
		t.Fatalf("bA-art: %s, want host", loc)
	}
}

// A multipart /complete that S3 refuses (a permanent error code, or else a
// 4xx) aborts the upload; one S3 fails (a 5xx) answers 502 and leaves the
// upload for the runner's retry or abort. The blob stays on its host
// either way.
func TestDirectUploadCompleteFailureAborts(t *testing.T) {
	for _, c := range []struct {
		name      string
		err       *s3Err
		code      int
		wantAbort bool
	}{
		{"unknown upload", nil, http.StatusInternalServerError, true},
		{"InvalidPart", &s3Err{http.StatusBadRequest, "InvalidPart"}, http.StatusInternalServerError, true},
		{"S3 500", &s3Err{http.StatusInternalServerError, "InternalError"}, http.StatusBadGateway, false},
		// An error in a 200 answer, which the SDK reports as a 500.
		{"embedded InvalidPart", &s3Err{http.StatusOK, "InvalidPart"}, http.StatusInternalServerError, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := reportFixture(t)
			f := useDirectS3(t, s)
			setBlob(t, s, "bA-art", []byte("artifact bytes"))
			f.completeErr = c.err
			code, body := runnerPost(t, s, "ha", "bA-art", "complete", proto.BlobCompleteRequest{UploadID: "up-unknown",
				Parts: []proto.UploadedPart{{Number: 1, ETag: `"x"`, SHA256: b64sum(nil)}}})
			if code != c.code {
				t.Fatalf("complete: %d %s, want %d", code, body, c.code)
			}
			if got := slices.Contains(f.Calls(), "DELETE AbortMultipartUpload "+blob.Key("t1", "ra", "bA-art")); got != c.wantAbort {
				t.Fatalf("aborted %v, want %v: %q", got, c.wantAbort, f.Calls())
			}
			if loc := blobLocations(t, s, "ra")["bA-art"]; loc != "host" {
				t.Fatalf("bA-art: %s, want host", loc)
			}
		})
	}
}

// /complete with abort drops that upload id's parts for the blob's key and
// records nothing.
func TestDirectUploadAbort(t *testing.T) {
	s, _ := reportFixture(t)
	f := useDirectS3(t, s)
	vol := bytes.Repeat([]byte("v"), proto.BlobPartSize+5)
	setBlob(t, s, "bA-vol", vol)
	out := startUpload(t, s, "bA-vol", vol)
	if code, _ := putPresigned(t, out.Parts[0].PresignedRequest, vol[:proto.BlobPartSize]); code != http.StatusOK {
		t.Fatalf("PUT part 1: %d", code)
	}
	code, body := runnerPost(t, s, "ha", "bA-vol", "complete", proto.BlobCompleteRequest{UploadID: out.UploadID, Abort: true})
	if code != http.StatusOK || strings.TrimSpace(body) != `{"aborted":true}` {
		t.Fatalf("abort: %d %s", code, body)
	}
	key := blob.Key("t1", "ra", "bA-vol")
	if !slices.Contains(f.Calls(), "DELETE AbortMultipartUpload "+key) {
		t.Fatalf("not aborted: %q", f.Calls())
	}
	f.mu.Lock()
	_, open := f.parts[out.UploadID]
	f.mu.Unlock()
	if open {
		t.Fatalf("upload %s still open", out.UploadID)
	}
	if loc := blobLocations(t, s, "ra")["bA-vol"]; loc != "host" {
		t.Fatalf("bA-vol: %s, want host", loc)
	}
}

// An artifact PUT straight to S3 whose /complete never comes (its runner
// gone) and then deleted: deleteArtifacts deletes its key.
func TestDeleteArtifactsDeletesUncompletedDirectUpload(t *testing.T) {
	s, ctx, _ := retentionFixture(t, StateTerminated, StateTerminated, 1, 1)
	f := useDirectS3(t, s)
	content := []byte("artifact bytes")
	hostArtifact(t, s, ctx, content)
	putSingle(t, s, "bA-art", content)
	if code, _ := callJSON(t, s, apiKey(t, s, new("t1"), "run"), http.MethodDelete, "/v1/runs/ra/artifacts"); code != http.StatusOK {
		t.Fatalf("delete: %d", code)
	}
	key := blob.Key("t1", "ra", "bA-art")
	if !slices.Contains(f.Deleted(), key) {
		t.Fatalf("S3 deletes %v, want %s", f.Deleted(), key)
	}
	f.mu.Lock()
	_, left := f.objects[key]
	f.mu.Unlock()
	if left {
		t.Fatalf("%s left in S3", key)
	}
}

// An artifact deleted after its presigned PUT, then the runner's retry
// asks /upload again: 410 gone, and the object the PUT wrote is deleted.
func TestDirectUploadStartOfDeletedBlob(t *testing.T) {
	s, ctx, _ := retentionFixture(t, StateTerminated, StateTerminated, 1, 1)
	f := useDirectS3(t, s)
	content := []byte("artifact bytes")
	hostArtifact(t, s, ctx, content)
	putSingle(t, s, "bA-art", content)
	if code, _ := callJSON(t, s, apiKey(t, s, new("t1"), "run"), http.MethodDelete, "/v1/runs/ra/artifacts"); code != http.StatusOK {
		t.Fatalf("delete: %d", code)
	}
	key := blob.Key("t1", "ra", "bA-art")
	before := len(f.Deleted())
	// The object written again after the delete's own key delete, as a
	// PUT racing the claim would.
	f.setObject(key, int64(len(content)))
	code, body := runnerPost(t, s, "ha", "bA-art", "upload", uploadRequest(content))
	if code != http.StatusGone || !strings.Contains(body, `"code":"gone"`) {
		t.Fatalf("upload again: %d %s, want 410 gone", code, body)
	}
	if got := f.Deleted()[before:]; !slices.Equal(got, []string{key}) {
		t.Fatalf("S3 deletes after /upload %v, want %s", got, key)
	}
}

// An artifact deleted during its direct upload, after the bytes reached
// S3: /complete answers 410, the object is deleted, the blob stays
// deleted. Both orders: the delete before /complete, and during it (after
// /complete read the blob, before its transaction).
func TestDeleteArtifactsDuringDirectUpload(t *testing.T) {
	for _, during := range []bool{false, true} {
		t.Run(map[bool]string{false: "before complete", true: "during complete"}[during], func(t *testing.T) {
			s, ctx, _ := retentionFixture(t, StateTerminated, StateTerminated, 1, 1)
			f := useDirectS3(t, s)
			content := []byte("artifact bytes")
			hostArtifact(t, s, ctx, content)
			apiK := apiKey(t, s, new("t1"), "run")
			del := func() {
				if code, _ := callJSON(t, s, apiK, http.MethodDelete, "/v1/runs/ra/artifacts"); code != http.StatusOK {
					t.Errorf("delete: %d", code)
				}
			}
			putSingle(t, s, "bA-art", content)
			key := blob.Key("t1", "ra", "bA-art")
			// After the delete (and its own key delete), the object is
			// written again, as a PUT racing the claim would: only
			// /complete's cleanup deletes it.
			before := -1
			delAndRewrite := func() {
				del()
				f.mu.Lock()
				f.objects[key] = int64(len(content))
				before = len(f.deleted)
				f.mu.Unlock()
			}
			if during {
				f.onHead = delAndRewrite
			} else {
				delAndRewrite()
			}
			if code, body := runnerPost(t, s, "ha", "bA-art", "complete", proto.BlobCompleteRequest{}); code != http.StatusGone {
				t.Fatalf("complete: %d %s, want 410", code, body)
			}
			if loc := blobLocations(t, s, "ra")["bA-art"]; loc != "deleted" {
				t.Fatalf("bA-art: %s, want deleted", loc)
			}
			f.mu.Lock()
			_, left := f.objects[key]
			after := slices.Clone(f.deleted[max(before, 0):])
			f.mu.Unlock()
			if before < 0 || !slices.Equal(after, []string{key}) {
				t.Fatalf("S3 deletes after the delete %v (from %d), want %s", after, before, key)
			}
			if left {
				t.Fatalf("%s left in S3", key)
			}
		})
	}
}
