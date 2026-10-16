package cli

import (
	"cmp"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// writeNetClasses prints network transfer per class, one line each, in
// classification order (provider:<region> keys by region under provider).
// unknown says what it is: it is not internet and not free.
func writeNetClasses(w io.Writer, net map[string]proto.NetBytes) {
	rank := func(k string) int {
		c, _, _ := strings.Cut(k, ":")
		if i := slices.Index(proto.NetClasses, proto.NetClass(c)); i >= 0 {
			return i
		}
		return len(proto.NetClasses)
	}
	keys := slices.SortedFunc(maps.Keys(net), func(a, b string) int {
		return cmp.Or(cmp.Compare(rank(a), rank(b)), strings.Compare(a, b))
	})
	for _, k := range keys {
		b := net[k]
		note := ""
		if k == string(proto.NetUnknown) {
			note = "  (unclassified: no provider catalog)"
		}
		delivered := ""
		if b.RxDelivered > b.Rx {
			delivered = "  (delivered " + spec.BytesText(b.RxDelivered) + ")"
		}
		fmt.Fprintf(w, "  %-22s rx %-10s tx %s%s%s\n", k, spec.BytesText(b.Rx), spec.BytesText(b.Tx), delivered, note)
	}
}
