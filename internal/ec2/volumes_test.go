package ec2

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/marcioapm/lux/internal/server"
)

// Volumes asks one DescribeVolumes for every id, filtered by
// attachment.instance-id, and keeps only the volumes deleted with their
// instance; an instance with an attachment but none such maps to empty, and
// one with no attachment is absent.
func TestVolumesFiltersByInstanceAndDeleteOnTermination(t *testing.T) {
	awsTestEnv(t)
	var calls []string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		calls = append(calls, fmt.Sprintf("%s %s %s,%s", r.PostForm.Get("Action"), r.PostForm.Get("Filter.1.Name"),
			r.PostForm.Get("Filter.1.Value.1"), r.PostForm.Get("Filter.1.Value.2")))
		w.Header().Set("Content-Type", "text/xml")
		vol := func(id, typ string, size, iops, tput int, inst string, del bool) string {
			return fmt.Sprintf(`<item><volumeId>%s</volumeId><volumeType>%s</volumeType><size>%d</size><iops>%d</iops><throughput>%d</throughput>`+
				`<attachmentSet><item><instanceId>%s</instanceId><deleteOnTermination>%t</deleteOnTermination></item></attachmentSet></item>`,
				id, typ, size, iops, tput, inst, del)
		}
		fmt.Fprint(w, `<DescribeVolumesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><volumeSet>`+
			vol("vol-1", "gp3", 100, 3000, 125, "i-a", true)+
			vol("vol-2", "io2", 50, 4000, 0, "i-a", true)+
			vol("vol-3", "gp3", 500, 3000, 125, "i-b", false)+
			`</volumeSet></DescribeVolumesResponse>`)
	}))
	defer fake.Close()
	p := New(fake.URL, discard)
	got, err := p.Volumes(context.Background(), json.RawMessage(`{"region":"eu-north-1"}`), []string{"i-a", "i-b", "i-c"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]server.HostVolume{
		"i-a": {{Type: "gp3", SizeGiB: 100, IOPS: 3000, ThroughputMiBps: 125}, {Type: "io2", SizeGiB: 50, IOPS: 4000}},
		"i-b": {},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("volumes %+v, want %+v", got, want)
	}
	if fmt.Sprint(calls) != "[DescribeVolumes attachment.instance-id i-a,i-b]" {
		t.Errorf("calls %q", calls)
	}
}

// A refused DescribeVolumes is an error, never an empty answer.
func TestVolumesError(t *testing.T) {
	awsTestEnv(t)
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ec2Error(w, http.StatusForbidden, "UnauthorizedOperation", "not authorized to perform ec2:DescribeVolumes")
	}))
	defer fake.Close()
	got, err := New(fake.URL, discard).Volumes(context.Background(), json.RawMessage(`{"region":"eu-north-1"}`), []string{"i-a"})
	if err == nil || got != nil {
		t.Errorf("got %v, %v; want an error", got, err)
	}
}
