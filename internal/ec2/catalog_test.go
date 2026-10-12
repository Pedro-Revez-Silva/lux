package ec2

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// describeReply is a DescribeInstanceTypes answer: m7i.large as EC2 gives
// it (nested virtualization, two network cards), t4g.small without any
// processor feature, burstable.
const describeReply = `<DescribeInstanceTypesResponse ` + ns + `><instanceTypeSet>
<item><instanceType>m7i.large</instanceType><burstablePerformanceSupported>false</burstablePerformanceSupported>
 <vCpuInfo><defaultVCpus>2</defaultVCpus></vCpuInfo><memoryInfo><sizeInMiB>8192</sizeInMiB></memoryInfo>
 <processorInfo><supportedArchitectures><item>x86_64</item></supportedArchitectures><supportedFeatures><item>amd-sev-snp</item><item>nested-virtualization</item></supportedFeatures></processorInfo>
 <networkInfo><networkPerformance>Up to 12.5 Gigabit</networkPerformance><networkCards><item><baselineBandwidthInGbps>0.75</baselineBandwidthInGbps></item><item><baselineBandwidthInGbps>0.5</baselineBandwidthInGbps></item></networkCards></networkInfo></item>
<item><instanceType>t4g.small</instanceType><burstablePerformanceSupported>true</burstablePerformanceSupported>
 <vCpuInfo><defaultVCpus>2</defaultVCpus></vCpuInfo><memoryInfo><sizeInMiB>2048</sizeInMiB></memoryInfo>
 <processorInfo><supportedArchitectures><item>arm64</item></supportedArchitectures></processorInfo></item>
</instanceTypeSet></DescribeInstanceTypesResponse>`

// InstanceTypes asks for every type in one call and records what EC2
// says; a type a launch already looked up (its memory) is not asked again,
// and a second catalog read within a day asks nothing.
func TestInstanceTypes(t *testing.T) {
	endpoint, forms := marketEC2(t, func(action string, form url.Values, w http.ResponseWriter) {
		if action != "DescribeInstanceTypes" {
			ec2Error(w, http.StatusBadRequest, "InvalidAction", action)
			return
		}
		fmt.Fprint(w, describeReply)
	})
	p := New(endpoint, discard)
	infos, err := p.InstanceTypes(context.Background(), "us-east-1", []string{"m7i.large", "t4g.small"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, in := range infos {
		b, _ := json.Marshal(in)
		got = append(got, string(b))
	}
	want := []string{
		`{"Name":"m7i.large","VCPUs":2,"MemoryBytes":8589934592,"Architectures":["x86_64"],"NestedVirtualization":true,"Burstable":false,"NetworkBaselineGbps":1.25,"NetworkPerformance":"Up to 12.5 Gigabit"}`,
		`{"Name":"t4g.small","VCPUs":2,"MemoryBytes":2147483648,"Architectures":["arm64"],"NestedVirtualization":false,"Burstable":true,"NetworkBaselineGbps":null,"NetworkPerformance":""}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("infos\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if f := (*forms)[0]; len(*forms) != 1 || f.Get("InstanceType.1") != "m7i.large" || f.Get("InstanceType.2") != "t4g.small" {
		t.Fatalf("requests %v", *forms)
	}

	// The launch path's memory lookup reads the same cache.
	c, err := p.client(context.Background(), "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	if mem, err := p.instanceMemory(context.Background(), c, "us-east-1", "t4g.small"); err != nil || mem != 2<<30 {
		t.Fatalf("memory %d %v", mem, err)
	}
	if _, err := p.InstanceTypes(context.Background(), "us-east-1", []string{"t4g.small", "m7i.large"}); err != nil || len(*forms) != 1 {
		t.Fatalf("cached read made %d requests (%v)", len(*forms), err)
	}
}
