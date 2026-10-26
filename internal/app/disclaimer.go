package app

import (
	"net/http"
	"time"
)

// The disclaimer is shown once per install, on first use, and stays in the
// footer after. Accepting it is recorded with its date; Wipe clears it.

const disclaimerKey = "disclaimerAccepted"

func (s *Server) postDisclaimer(w http.ResponseWriter, r *http.Request) {
	v := time.Now().UTC().Format(time.RFC3339)
	if err := s.st.SetSetting(disclaimerKey, v); err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]string{"accepted": v})
}

// packageDisclaimer heads the README in the accountant package.
const packageDisclaimer = `This package was prepared with Proof of Basis, a self-help software tool
for organizing records. It is not tax, legal or accounting advice, and
Proof of Basis is not a CPA firm, law firm or tax preparer. The figures come from the
user's own records and outside price data and may be incomplete or wrong;
please verify them before relying on them. The taxpayer remains responsible
for their return.
`
