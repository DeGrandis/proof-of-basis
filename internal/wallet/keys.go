// Package wallet turns extended public keys (xpub/ypub/zpub and their
// testnet forms) into addresses and Electrum script hashes.
package wallet

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/base58"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
)

// ScriptType is the output script a wallet uses for its addresses.
type ScriptType string

const (
	P2PKH      ScriptType = "p2pkh"       // legacy, BIP44
	P2SHP2WPKH ScriptType = "p2sh-p2wpkh" // nested segwit, BIP49
	P2WPKH     ScriptType = "p2wpkh"      // native segwit, BIP84
	P2TR       ScriptType = "p2tr"        // taproot key path, BIP86
)

func (s ScriptType) Valid() bool {
	switch s {
	case P2PKH, P2SHP2WPKH, P2WPKH, P2TR:
		return true
	}
	return false
}

type versionInfo struct {
	prefix  string
	script  ScriptType
	mainnet bool
}

// SLIP-0132 version bytes for single-sig extended public keys.
var versions = map[[4]byte]versionInfo{
	{0x04, 0x88, 0xb2, 0x1e}: {"xpub", P2PKH, true},
	{0x04, 0x9d, 0x7c, 0xb2}: {"ypub", P2SHP2WPKH, true},
	{0x04, 0xb2, 0x47, 0x46}: {"zpub", P2WPKH, true},
	{0x04, 0x35, 0x87, 0xcf}: {"tpub", P2PKH, false},
	{0x04, 0x4a, 0x52, 0x62}: {"upub", P2SHP2WPKH, false},
	{0x04, 0x5f, 0x1c, 0xf6}: {"vpub", P2WPKH, false},
}

var (
	xpubVersion = []byte{0x04, 0x88, 0xb2, 0x1e}
	tpubVersion = []byte{0x04, 0x35, 0x87, 0xcf}
)

// Account is a parsed extended public key at the account level.
type Account struct {
	key    *hdkeychain.ExtendedKey
	Script ScriptType
	Params *chaincfg.Params
	Prefix string
}

// ErrMultisig is returned for Ypub/Zpub style keys on their own: they are one
// cosigner of a multisig wallet, which needs every cosigner's key.
var ErrMultisig = errors.New("this is one cosigner's key from a multisig wallet: choose Multisig and paste every cosigner's key, or paste the wallet's output descriptor")

// SLIP-0132 version bytes for multisig cosigner keys (Ypub, Zpub, Upub, Vpub).
var multisigVersions = map[[4]byte]bool{
	{0x02, 0x95, 0xb4, 0x3f}: true, {0x02, 0xaa, 0x7e, 0xd3}: true, // Ypub, Zpub
	{0x02, 0x42, 0x89, 0xef}: true, {0x02, 0x57, 0x54, 0x83}: true, // Upub, Vpub
}

// ParseExtendedKey parses an xpub/ypub/zpub (or tpub/upub/vpub). The script
// type is inferred from the prefix; override replaces it when non-empty,
// because many wallets export a plain xpub for segwit accounts. regtest
// selects regtest address encoding for testnet-prefixed keys.
func ParseExtendedKey(s string, override ScriptType, regtest bool) (*Account, error) {
	return parseExtendedKey(s, override, regtest, false)
}

// parseExtendedKey also accepts multisig cosigner keys when allowMulti is
// set, for use inside multisig descriptors.
func parseExtendedKey(s string, override ScriptType, regtest, allowMulti bool) (*Account, error) {
	s = strings.TrimSpace(s)
	raw := base58.Decode(s)
	if len(raw) != 82 {
		return nil, fmt.Errorf("not an extended key (decoded length %d)", len(raw))
	}
	payload, sum := raw[:78], raw[78:]
	if !bytes.Equal(chainhash.DoubleHashB(payload)[:4], sum) {
		return nil, errors.New("extended key checksum mismatch")
	}
	var ver [4]byte
	copy(ver[:], payload[:4])
	info, ok := versions[ver]
	if !ok && multisigVersions[ver] {
		if !allowMulti {
			return nil, ErrMultisig
		}
		mainnet := ver == [4]byte{0x02, 0x95, 0xb4, 0x3f} || ver == [4]byte{0x02, 0xaa, 0x7e, 0xd3}
		info, ok = versionInfo{"multisig", P2PKH, mainnet}, true
	}
	if !ok {
		return nil, fmt.Errorf("unsupported extended key version %x (private keys are never accepted)", ver)
	}

	// Normalise to xpub/tpub so hdkeychain derives it without network checks.
	norm := make([]byte, 78)
	copy(norm, payload)
	params := &chaincfg.MainNetParams
	if info.mainnet {
		copy(norm, xpubVersion)
	} else {
		copy(norm, tpubVersion)
		params = &chaincfg.TestNet3Params
		if regtest {
			params = &chaincfg.RegressionNetParams
		}
	}
	enc := base58.Encode(append(norm, chainhash.DoubleHashB(norm)[:4]...))
	key, err := hdkeychain.NewKeyFromString(enc)
	if err != nil {
		return nil, err
	}
	if key.IsPrivate() {
		return nil, errors.New("private keys are never accepted")
	}
	script := info.script
	if override != "" {
		if !override.Valid() {
			return nil, fmt.Errorf("unknown script type %q", override)
		}
		script = override
	}
	return &Account{key: key, Script: script, Params: params, Prefix: info.prefix}, nil
}

// Address is one derived address and the data needed to query it.
type Address struct {
	Chain        uint32 // 0 receive, 1 change
	Index        uint32
	Address      string
	ScriptPubKey []byte
	ScriptHash   string // Electrum scripthash: sha256(scriptPubKey), byte-reversed, hex
}

// Derive returns the address at m/<account>/chain/index.
func (a *Account) Derive(chain, index uint32) (Address, error) {
	c, err := a.key.Derive(chain)
	if err != nil {
		return Address{}, err
	}
	k, err := c.Derive(index)
	if err != nil {
		return Address{}, err
	}
	pub, err := k.ECPubKey()
	if err != nil {
		return Address{}, err
	}
	addr, err := a.addressFor(pub)
	if err != nil {
		return Address{}, err
	}
	spk, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return Address{}, err
	}
	return Address{chain, index, addr.EncodeAddress(), spk, ScriptHash(spk)}, nil
}

func (a *Account) addressFor(pub *btcec.PublicKey) (btcutil.Address, error) {
	compressed := pub.SerializeCompressed()
	switch a.Script {
	case P2PKH:
		return btcutil.NewAddressPubKeyHash(btcutil.Hash160(compressed), a.Params)
	case P2WPKH:
		return btcutil.NewAddressWitnessPubKeyHash(btcutil.Hash160(compressed), a.Params)
	case P2SHP2WPKH:
		redeem := append([]byte{0x00, 0x14}, btcutil.Hash160(compressed)...)
		return btcutil.NewAddressScriptHash(redeem, a.Params)
	case P2TR:
		tweaked := txscript.ComputeTaprootKeyNoScript(pub)
		return btcutil.NewAddressTaproot(schnorr.SerializePubKey(tweaked), a.Params)
	}
	return nil, fmt.Errorf("unknown script type %q", a.Script)
}

// ScriptHash computes the Electrum protocol script hash for a scriptPubKey.
func ScriptHash(spk []byte) string {
	h := sha256.Sum256(spk)
	for i, j := 0, len(h)-1; i < j; i, j = i+1, j-1 {
		h[i], h[j] = h[j], h[i]
	}
	return hex.EncodeToString(h[:])
}
