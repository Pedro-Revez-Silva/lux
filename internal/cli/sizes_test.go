package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/server"
)

// mShape is the shape of the fake luxd's pool "m", as GET /v1/pools lists it.
const mShape = `"shape":{"family":"m","arch":"arm64","cpus":8,"memory":34359738368,"disk":171798691840,"minCpus":0.5,"defaultCpus":2},
	"sizes":[{"name":"m-05-arm64","cpus":0.5,"memory":2147483648,"disk":10737418240,"fraction":0.0625},
		{"name":"m-1-arm64","cpus":1,"memory":4294967296,"disk":21474836480,"fraction":0.125},
		{"name":"m-2-arm64","cpus":2,"memory":8589934592,"disk":42949672960,"fraction":0.25,"default":true},
		{"name":"m-4-arm64","cpus":4,"memory":17179869184,"disk":85899345920,"fraction":0.5},
		{"name":"m-8-arm64","cpus":8,"memory":34359738368,"disk":171798691840,"fraction":1}]`

// --size sets resources.size and nothing else of resources.
func TestRunSizeFlag(t *testing.T) {
	var posted []map[string]any
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		posted = append(posted, body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"run_1","state":"submitted"}`))
	})
	out, err := runCLI(t, h, "run", "--image", "alpine", "--pool", "m", "--size", "m-2-arm64", "--", "true")
	if err != nil || out != "run_1\n" {
		t.Fatalf("run: %q %v", out, err)
	}
	if res := posted[0]["resources"]; !reflect.DeepEqual(res, map[string]any{"size": "m-2-arm64"}) {
		t.Errorf("POSTed resources %v, want only size", res)
	}
	posted = nil
	if _, err := runCLI(t, h, "run", "--image", "alpine", "--", "true"); err != nil {
		t.Fatal(err)
	}
	if res := posted[0]["resources"]; !reflect.DeepEqual(res, map[string]any{}) {
		t.Errorf("without --size: resources %v, want none", res)
	}
}

// resources.size in a spec file is posted; --size replaces it.
func TestRunSizeFromSpecFile(t *testing.T) {
	var posted []map[string]any
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		posted = append(posted, body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"run_1","state":"submitted"}`))
	})
	file := filepath.Join(t.TempDir(), "run.yaml")
	yml := "image: {ref: alpine}\nworkload: {adapter: generic, command: [\"true\"]}\nresources: {size: m-2-arm64}\n"
	if err := os.WriteFile(file, []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, h, "run", "-f", file); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, h, "run", "-f", file, "--size", "m-4-arm64"); err != nil {
		t.Fatal(err)
	}
	if len(posted) != 2 {
		t.Fatalf("posted %d", len(posted))
	}
	for i, want := range []string{"m-2-arm64", "m-4-arm64"} {
		if res := posted[i]["resources"]; !reflect.DeepEqual(res, map[string]any{"size": want}) {
			t.Errorf("POST %d resources %v, want only size %s", i, res, want)
		}
	}
}

// --size sends resources.size, with --pool the move; the answer's size is
// printed on stderr.
func TestResumeSizeFlag(t *testing.T) {
	var posted []map[string]any
	h := poolsAndResume(t, http.StatusAccepted, `{"id":"run_1","state":"resuming","resize":{"requested":{"size":"m-4-arm64"},
		"applied":{"cpus":4,"memory":17179869184,"disk":85899345920},"size":"m-4-arm64"}}`, &posted)
	out, errOut, err := runCLIErr(t, h, "resume", "run_1", "--secret", "TOKEN=x", "--pool", "gpu", "--size", "m-4-arm64")
	if err != nil {
		t.Fatal(err)
	}
	if len(posted) != 1 || !reflect.DeepEqual(posted[0]["resources"], map[string]any{"size": "m-4-arm64"}) ||
		!reflect.DeepEqual(posted[0]["placement"], map[string]any{"poolId": "pool_gpu"}) {
		t.Errorf("POSTed %v", posted)
	}
	if out != "run_1 resuming\n" || !strings.Contains(errOut, "size: m-4-arm64\n") {
		t.Errorf("stdout %q stderr %q", out, errOut)
	}
}

// pools sizes lists a shaped pool's ladder, the default starred; -o json
// the sizes; an unshaped pool is an error naming it.
func TestPoolsSizes(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"pools":[{"id":"pool_m","name":"m","provider":"ec2",` + mShape + `},{"id":"pool_p","name":"plain","provider":"static"}]}`))
	})
	out, err := runCLI(t, h, "pools", "sizes", "m")
	if err != nil {
		t.Fatal(err)
	}
	want := "SIZE        DEFAULT  CPUS  MEMORY    DISK       FRACTION\n" +
		"m-05-arm64           0.5   2.0 GiB   10.0 GiB   0.0625\n" +
		"m-1-arm64            1     4.0 GiB   20.0 GiB   0.125\n" +
		"m-2-arm64   *        2     8.0 GiB   40.0 GiB   0.25\n" +
		"m-4-arm64            4     16.0 GiB  80.0 GiB   0.5\n" +
		"m-8-arm64            8     32.0 GiB  160.0 GiB  1\n"
	if out != want {
		t.Errorf("table:\n%s\nwant:\n%s", out, want)
	}
	out, err = runCLI(t, h, "-o", "json", "pools", "sizes", "pool_m")
	var sizes []server.PoolSize
	if err != nil || json.Unmarshal([]byte(out), &sizes) != nil {
		t.Fatalf("json: %q %v", out, err)
	}
	const g = 1 << 30
	wantSizes := []server.PoolSize{
		{Name: "m-05-arm64", CPUs: 0.5, Memory: 2 * g, Disk: 10 * g, Fraction: 0.0625},
		{Name: "m-1-arm64", CPUs: 1, Memory: 4 * g, Disk: 20 * g, Fraction: 0.125},
		{Name: "m-2-arm64", CPUs: 2, Memory: 8 * g, Disk: 40 * g, Fraction: 0.25, Default: true},
		{Name: "m-4-arm64", CPUs: 4, Memory: 16 * g, Disk: 80 * g, Fraction: 0.5},
		{Name: "m-8-arm64", CPUs: 8, Memory: 32 * g, Disk: 160 * g, Fraction: 1},
	}
	if !reflect.DeepEqual(sizes, wantSizes) {
		t.Errorf("json sizes %+v, want %+v", sizes, wantSizes)
	}
	for _, args := range [][]string{{"pools", "sizes", "plain"}, {"-o", "json", "pools", "sizes", "plain"}} {
		out, err := runCLI(t, h, args...)
		if want := "pool plain has no shape, so no sizes: Runs there ask with --cpus, --memory and --disk"; err == nil || err.Error() != want || out != "" {
			t.Errorf("%v: %q %v, want no output and %q", args, out, err, want)
		}
	}
	if _, err := runCLI(t, h, "pools", "sizes", "nope"); err == nil || err.Error() != `no pool "nope"` {
		t.Errorf("unknown: %v", err)
	}
}

// sizesLuxd lists pools as GET /v1/pools would: those of ?tenant= (by
// name) and the platform's when it is given, else all of them.
func sizesLuxd(pools ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		tenant := r.URL.Query().Get("tenant")
		var listed []string
		for _, p := range pools {
			var pl struct {
				Tenant   string `json:"tenant"`
				Platform bool   `json:"platform"`
			}
			_ = json.Unmarshal([]byte(p), &pl)
			if tenant == "" || pl.Platform || pl.Tenant == tenant {
				listed = append(listed, p)
			}
		}
		_, _ = w.Write([]byte(`{"pools":[` + strings.Join(listed, ",") + `]}`))
	})
}

// sizeNames is the size names of pools sizes -o json's output.
func sizeNames(t *testing.T, out string) []string {
	t.Helper()
	var sizes []server.PoolSize
	if err := json.Unmarshal([]byte(out), &sizes); err != nil {
		t.Fatalf("json %q: %v", out, err)
	}
	var names []string
	for _, s := range sizes {
		names = append(names, s.Name)
	}
	return names
}

// pools sizes picks the tenant's pool of a name over the platform's, the
// platform's with --platform; an operator listing several tenants' pools
// of that name is refused until --tenant names one.
func TestPoolsSizesPicksThePool(t *testing.T) {
	ladder := func(names ...string) string {
		var s []string
		for _, n := range names {
			s = append(s, `{"name":"`+n+`","cpus":1,"memory":1,"disk":1,"fraction":1}`)
		}
		return `"shape":{"family":"x"},"sizes":[` + strings.Join(s, ",") + `]`
	}
	tenantM := `{"id":"pool_t1m","name":"m","tenant":"t1",` + ladder("t1-size") + `}`
	otherM := `{"id":"pool_t2m","name":"m","tenant":"t2",` + ladder("t2-size") + `}`
	platformM := `{"id":"pool_pm","name":"m","platform":true,` + ladder("platform-size") + `}`

	h := sizesLuxd(tenantM, platformM)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"pools", "sizes", "m"}, "t1-size"},
		{[]string{"pools", "sizes", "m", "--platform"}, "platform-size"},
		{[]string{"pools", "sizes", "pool_t1m"}, "t1-size"},
		{[]string{"pools", "sizes", "pool_pm"}, "platform-size"},
	} {
		out, err := runCLI(t, h, append([]string{"-o", "json"}, c.args...)...)
		if err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		if got := sizeNames(t, out); !reflect.DeepEqual(got, []string{c.want}) {
			t.Errorf("%v: sizes %v, want [%s]", c.args, got, c.want)
		}
	}
	if _, err := runCLI(t, h, "pools", "sizes", "pool_t1m", "--platform"); err == nil || err.Error() != `no pool "pool_t1m"` {
		t.Errorf("--platform with a tenant pool's id: %v", err)
	}

	h = sizesLuxd(tenantM, otherM, platformM)
	if _, err := runCLI(t, h, "pools", "sizes", "m"); err == nil || err.Error() != `pool "m": tenants t1, t2 each have one: name one with --tenant` {
		t.Errorf("operator, ambiguous: %v", err)
	}
	out, err := runCLI(t, h, "--tenant", "t2", "-o", "json", "pools", "sizes", "m")
	if err != nil {
		t.Fatal(err)
	}
	if got := sizeNames(t, out); !reflect.DeepEqual(got, []string{"t2-size"}) {
		t.Errorf("--tenant t2: sizes %v, want t2's", got)
	}
	out, err = runCLI(t, h, "-o", "json", "pools", "sizes", "m", "--platform")
	if err != nil {
		t.Fatal(err)
	}
	if got := sizeNames(t, out); !reflect.DeepEqual(got, []string{"platform-size"}) {
		t.Errorf("operator --platform: sizes %v", got)
	}
}

// pools set --shape sends the parsed shape; a malformed one is refused
// before anything is sent; a set leaving out a pool's shape is a removal.
func TestPoolsSetShape(t *testing.T) {
	url, posted := poolsLuxd(t, false)
	code, _, stderr := lux(url, "pools", "set", "m", "--provider", "static",
		"--shape", "family=m,arch=arm64,cpus=8,memory=32Gi,disk=160Gi,min-cpus=0.5,default-cpus=2")
	if code != 0 || len(*posted) != 1 {
		t.Fatalf("exit %d (%s), posted %v", code, stderr, *posted)
	}
	want := map[string]any{"family": "m", "arch": "arm64", "cpus": 8.0, "memory": float64(32 << 30), "disk": float64(160 << 30), "minCpus": 0.5, "defaultCpus": 2.0}
	if got := (*posted)[0]["shape"]; !reflect.DeepEqual(got, want) {
		t.Errorf("shape %v, want %v", got, want)
	}
	if code, _, stderr = lux(url, "pools", "set", "m", "--provider", "static", "--shape", "family=m,arch=arm64"); code == 0 || !strings.Contains(stderr, "missing cpus") || len(*posted) != 1 {
		t.Errorf("malformed: exit %d, stderr %q, posted %d", code, stderr, len(*posted))
	}

	url, posted = poolsLuxd(t, false, `{"name":"m","tenant":"t1","provider":"static",`+mShape+`}`)
	code, stdout, stderr := lux(url, "pools", "set", "m", "--provider", "static")
	if code != 4 || len(*posted) != 0 || !strings.Contains(stderr, "would remove shape.arch, shape.cpus") || !strings.Contains(stdout, "shape.family m→-") {
		t.Errorf("removal: exit %d, posted %v, stdout %q, stderr %q", code, *posted, stdout, stderr)
	}
}

// get prints a shaped Run's size and what its submit asked for; none for
// an unshaped Run.
func TestGetPrintsSize(t *testing.T) {
	run := `{"id":"run_1","state":"queued","spec":{"workload":{"adapter":"generic"}},"createdAt":"2026-10-01T00:00:00Z"`
	for _, c := range []struct{ body, want string }{
		{run + `,"sizing":{"requested":{"cpus":3,"memory":10737418240},"resolved":{"size":"m-4-arm64","cpus":4,"memory":17179869184,"disk":85899345920}}}`,
			"size:      m-4-arm64 (4 cpus, 16.0 GiB memory, 80.0 GiB disk); asked 3 cpus, 10.0 GiB memory\n"},
		{run + `,"sizing":{"requested":{},"resolved":{"size":"m-2-arm64","cpus":2,"memory":8589934592,"disk":42949672960}}}`,
			"size:      m-2-arm64 (2 cpus, 8.0 GiB memory, 40.0 GiB disk); asked the pool's default\n"},
		{run + `}`, ""},
	} {
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(c.body))
		})
		out, err := runCLI(t, h, "get", "run_1")
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		for _, l := range strings.SplitAfter(out, "\n") {
			if strings.HasPrefix(l, "size:") {
				got = l
			}
		}
		if got != c.want {
			t.Errorf("size line %q, want %q", got, c.want)
		}
	}
}
