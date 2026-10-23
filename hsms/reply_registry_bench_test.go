package hsms

import "testing"

// BenchmarkReplyRegistry_Transaction measures the registry's share of one W-bit transaction:
// register the primary, route its conforming reply, receive it, and deregister.
// allocs/op is the number to watch:
// it is paid once per synchronous request on every connection.
func BenchmarkReplyRegistry_Transaction(b *testing.B) {
	r := newReplyRegistry()
	key := [4]byte{0, 0, 0, 1}
	reply := replyResult{msg: replyDM(1, 2)}

	b.ReportAllocs()
	for b.Loop() {
		ch := r.registerData(sentHeader(key, 1, 1))
		if delivered, _ := r.route(key, reply, false); !delivered {
			b.Fatal("conforming reply was not delivered")
		}
		<-ch
		r.deregister(key)
	}
}
