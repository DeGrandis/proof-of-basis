package app

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// Files that leave the app (the accountant package, the Form 8949 and
// reconciliation CSVs) carry no on-chain data by default: no txids, no
// addresses, no record keys that embed a txid. Records are named by a
// reference (R-1A2B3C4D5E) that only this app can tie back to the
// record: it's a keyed hash under a secret kept in the database, so no
// one can test candidate txids against it. The app shows each record's
// reference and finds a record from it. Including on-chain ids is an
// explicit choice per download, for an audit.

type exportIDs struct {
	onchain bool
	secret  []byte
}

func (s *Server) exportIDs(onchain bool) exportIDs {
	sec := s.st.Setting("exportSecret", "")
	if b, err := hex.DecodeString(sec); err == nil && len(b) == 32 {
		return exportIDs{onchain, b}
	}
	b := make([]byte, 32)
	rand.Read(b)
	s.st.SetSetting("exportSecret", hex.EncodeToString(b))
	return exportIDs{onchain, b}
}

// ref is a record's private reference, the same in every export.
func (x exportIDs) ref(key string) string {
	m := hmac.New(sha256.New, x.secret)
	m.Write([]byte(key))
	return "R-" + strings.ToUpper(hex.EncodeToString(m.Sum(nil)[:5]))
}

// rec names a record in an export.
func (x exportIDs) rec(key string) string {
	if x.onchain {
		return key
	}
	return x.ref(key)
}

// tx is a transaction id, only when on-chain ids are included.
func (x exportIDs) tx(txid string) string {
	if x.onchain {
		return txid
	}
	return ""
}

var hex64 = regexp.MustCompile(`(?i)\b[0-9a-f]{64}\b`)

// platform is an exchange's own transaction id: kept, since it isn't on
// chain and matches what the exchange reports, unless it is a txid.
func (x exportIDs) platform(id string) string {
	if !x.onchain && hex64.MatchString(id) {
		return ""
	}
	return id
}
