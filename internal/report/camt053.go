// Package report renders closed windows for treasury systems.
package report

import (
	"encoding/xml"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/SetOff-Org/setoff-clearing/internal/clearing"
	"github.com/SetOff-Org/setoff-clearing/internal/netting"
)

// Namespace of the camt.053 version produced.
const Namespace = "urn:iso:std:iso:20022:tech:xsd:camt.053.001.08"

// Options control a report.
type Options struct {
	// Decimals per asset: amounts are integers in the asset's smallest unit,
	// camt amounts are decimal. Assets not listed have none.
	Decimals map[string]int
	// Participant limits the report to one participant's statements ("" for all).
	Participant string
	// Now stamps the message.
	Now time.Time
}

// Camt053 renders a closed window as an ISO 20022 BankToCustomerStatement:
// one statement per participant and asset, with each obligation as a booked
// entry and the net position as the closing balance (the opening balance is
// zero: a window starts clean).
//
// camt currencies are three-letter codes. Assets that are not (USDC, for
// instance) are reported under XXX, with the asset named in the statement's
// additional information.
func Camt053(w *clearing.Closed, opt Options) ([]byte, error) {
	type key struct{ participant, asset string }
	entries := map[key][]netting.Obligation{}
	for _, o := range w.Obligations {
		for _, p := range []string{o.Debtor, o.Creditor} {
			if opt.Participant == "" || opt.Participant == p {
				k := key{p, o.Asset}
				entries[k] = append(entries[k], o)
			}
		}
	}
	net := map[key]*big.Int{}
	for _, p := range w.Netting.Positions {
		net[key{p.Participant, p.Asset}] = &p.Net.Int
	}
	keys := make([]key, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].participant != keys[j].participant {
			return keys[i].participant < keys[j].participant
		}
		return keys[i].asset < keys[j].asset
	})

	closed := w.ClosedAt.UTC().Format(time.RFC3339)
	doc := document{Xmlns: Namespace, Stmt: bankToCustomer{
		GrpHdr: groupHeader{MsgID: fmt.Sprintf("SETOFF-W%d", w.Window), CreDtTm: opt.Now.UTC().Format(time.RFC3339)},
	}}
	for _, k := range keys {
		ccy, info := currency(k.asset)
		decimals := opt.Decimals[k.asset]
		closing := new(big.Int)
		if n := net[k]; n != nil {
			closing.Set(n)
		}
		st := statement{
			ID:           fmt.Sprintf("W%d-%s-%s", w.Window, k.participant, k.asset),
			ElctrncSeqNb: w.Window,
			CreDtTm:      closed,
			Acct:         account{ID: accountID{Othr: other{ID: k.participant}}, Ccy: ccy},
			Bal: []balance{
				bal("OPBD", ccy, new(big.Int), decimals, closed),
				bal("CLBD", ccy, closing, decimals, closed),
			},
			AddtlStmtInf: info,
		}
		for _, o := range entries[k] {
			ind := "CRDT"
			if o.Debtor == k.participant {
				ind = "DBIT"
			}
			st.Ntry = append(st.Ntry, entry{
				Amt:       amount{Ccy: ccy, Value: decimal(&o.Amount.Int, decimals)},
				CdtDbtInd: ind,
				Sts:       status{Cd: "BOOK"},
				BookgDt:   dateTime{DtTm: closed},
				BkTxCd:    bankTxCode{Prtry: proprietary{Cd: "SETOFF-OBLIGATION"}},
				NtryDtls: entryDetails{TxDtls: txDetails{
					Refs:      refs{EndToEndID: o.ID},
					RltdPties: parties{Dbtr: party{Nm: o.Debtor}, Cdtr: party{Nm: o.Creditor}},
				}},
			})
		}
		doc.Stmt.Stmt = append(doc.Stmt.Stmt, st)
	}
	out, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), append(out, '\n')...), nil
}

var iso4217 = regexp.MustCompile(`^[A-Z]{3}$`)

func currency(asset string) (string, string) {
	if iso4217.MatchString(asset) {
		return asset, ""
	}
	return "XXX", "asset=" + asset
}

func bal(code, ccy string, v *big.Int, decimals int, at string) balance {
	ind := "CRDT"
	if v.Sign() < 0 {
		ind = "DBIT"
	}
	return balance{
		Tp:        balanceType{CdOrPrtry: code2{Cd: code}},
		Amt:       amount{Ccy: ccy, Value: decimal(new(big.Int).Abs(v), decimals)},
		CdtDbtInd: ind,
		Dt:        dateTime{DtTm: at},
	}
}

// decimal renders v / 10^decimals without losing precision.
func decimal(v *big.Int, decimals int) string {
	s := new(big.Int).Abs(v).String()
	if decimals <= 0 {
		return s
	}
	if len(s) <= decimals {
		s = strings.Repeat("0", decimals-len(s)+1) + s
	}
	whole, frac := s[:len(s)-decimals], strings.TrimRight(s[len(s)-decimals:], "0")
	if frac == "" {
		return whole
	}
	return whole + "." + frac
}

type document struct {
	XMLName xml.Name       `xml:"Document"`
	Xmlns   string         `xml:"xmlns,attr"`
	Stmt    bankToCustomer `xml:"BkToCstmrStmt"`
}

type bankToCustomer struct {
	GrpHdr groupHeader `xml:"GrpHdr"`
	Stmt   []statement `xml:"Stmt"`
}

type groupHeader struct {
	MsgID   string `xml:"MsgId"`
	CreDtTm string `xml:"CreDtTm"`
}

type statement struct {
	ID           string    `xml:"Id"`
	ElctrncSeqNb uint64    `xml:"ElctrncSeqNb"`
	CreDtTm      string    `xml:"CreDtTm"`
	Acct         account   `xml:"Acct"`
	Bal          []balance `xml:"Bal"`
	Ntry         []entry   `xml:"Ntry"`
	AddtlStmtInf string    `xml:"AddtlStmtInf,omitempty"`
}

type account struct {
	ID  accountID `xml:"Id"`
	Ccy string    `xml:"Ccy"`
}

type accountID struct {
	Othr other `xml:"Othr"`
}

type other struct {
	ID string `xml:"Id"`
}

type balance struct {
	Tp        balanceType `xml:"Tp"`
	Amt       amount      `xml:"Amt"`
	CdtDbtInd string      `xml:"CdtDbtInd"`
	Dt        dateTime    `xml:"Dt"`
}

type balanceType struct {
	CdOrPrtry code2 `xml:"CdOrPrtry"`
}

type code2 struct {
	Cd string `xml:"Cd"`
}

type amount struct {
	Ccy   string `xml:"Ccy,attr"`
	Value string `xml:",chardata"`
}

type dateTime struct {
	DtTm string `xml:"DtTm"`
}

type entry struct {
	Amt       amount       `xml:"Amt"`
	CdtDbtInd string       `xml:"CdtDbtInd"`
	Sts       status       `xml:"Sts"`
	BookgDt   dateTime     `xml:"BookgDt"`
	BkTxCd    bankTxCode   `xml:"BkTxCd"`
	NtryDtls  entryDetails `xml:"NtryDtls"`
}

type status struct {
	Cd string `xml:"Cd"`
}

type bankTxCode struct {
	Prtry proprietary `xml:"Prtry"`
}

type proprietary struct {
	Cd string `xml:"Cd"`
}

type entryDetails struct {
	TxDtls txDetails `xml:"TxDtls"`
}

type txDetails struct {
	Refs      refs    `xml:"Refs"`
	RltdPties parties `xml:"RltdPties"`
}

type refs struct {
	EndToEndID string `xml:"EndToEndId"`
}

type parties struct {
	Dbtr party `xml:"Dbtr>Pty"`
	Cdtr party `xml:"Cdtr>Pty"`
}

type party struct {
	Nm string `xml:"Nm"`
}
