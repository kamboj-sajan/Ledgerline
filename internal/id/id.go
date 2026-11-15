// Package id generates prefixed random identifiers such as "pay_7Q3MZ…".
package id

import "crypto/rand"

// New returns prefix + "_" + 26 random base32 characters (130 bits from
// crypto/rand).
//
// The prefix makes an ID self-describing in logs and API responses, so
// passing a refund ID where a payment ID belongs is an obvious mistake.
// The IDs are random rather than time-ordered; at Ledgerline's scale the
// scattered B-tree inserts this causes do not matter.
func New(prefix string) string {
	return prefix + "_" + rand.Text()
}
