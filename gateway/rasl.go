package gateway

import (
	"mime"
	"net/http"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node/target"
	"github.com/whyrusleeping/nu/rasl"
)

// RASL (https://dasl.ing/rasl.html, D21, backlog 0052): GET/HEAD /.well-known/rasl/<cid>
// serves a held object's bytes as stored (raw blob or DRISL node), always
// application/octet-stream with nosniff, as a download, cacheable forever, with Range.
// It is stateless (no cookies, credentials or content negotiation; the query string is
// ignored) and answers only for content already held and complete (P4: an unheld CID is
// 404 and nothing is fetched). CORS is not added on this loopback listener (P5, N29):
// a public listener may add Access-Control-Allow-Origin: * so browsers can verify by
// hash, but on loopback that would let any page enumerate held content.

func (h *Handler) rasl(w http.ResponseWriter, r *http.Request) {
	c := r.PathValue("cid")
	if !target.IsCID(c) {
		writeError(w, apiv0.Errorf(apiv0.CodeInvalidArgument, "%q is not a DASL CID", c).WithAction("use "+rasl.WellKnown+"<cid>"))
		return
	}
	oc, ok := h.content.(ObjectContent)
	if !ok || h.content == nil {
		writeError(w, apiv0.NotImplemented("RASL on this gateway"))
		return
	}
	if err := h.allowed(c); err != nil {
		writeError(w, err)
		return
	}
	f, err := oc.OpenObject(r.Context(), c)
	if err != nil {
		writeError(w, err)
		return
	}
	defer f.Close()
	hdr := w.Header()
	hdr.Set("Content-Type", "application/octet-stream")
	hdr.Set("Cache-Control", immutableCache)
	hdr.Set("ETag", `"`+c+`"`)
	hdr.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": c}))
	if h.pub != nil {
		// P5: on the public listener browsers may read RASL bytes cross-origin and verify
		// them by hash. Only this route, only here.
		hdr.Set("Access-Control-Allow-Origin", "*")
	}
	boundRange(r)
	http.ServeContent(w, r, "", time.Time{}, f)
}
