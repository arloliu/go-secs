package gem

import "github.com/arloliu/go-secs/v2/secs2"

// Report builds one S6F11 report element: L[2]{ <rptid> L[b]{ <v>... } } (SEMI E5 §10.8).
//
// rptid is an equipment-defined report ID item (caller controls SECS-II type). values are the
// data values associated with the report. Pass the returned [secs2.Item] as a variadic argument
// to [S6F11].
func Report(rptid secs2.Item, values ...secs2.Item) secs2.Item {
	return secs2.L(rptid, secs2.L(values...))
}
