package model_proxy

import "math/rand/v2"

// These fixed labels are placeholders for mapped thinking responses. Replace
// individual entries here when different reserved values are needed. They are
// independent of native traffic and do not authenticate the reasoning summary.
var reservedThinkingSignatures = [...]string{
	"transithub-thinking-signature-01",
	"transithub-thinking-signature-02",
	"transithub-thinking-signature-03",
	"transithub-thinking-signature-04",
	"transithub-thinking-signature-05",
	"transithub-thinking-signature-06",
	"transithub-thinking-signature-07",
	"transithub-thinking-signature-08",
	"transithub-thinking-signature-09",
	"transithub-thinking-signature-10",
	"transithub-thinking-signature-11",
	"transithub-thinking-signature-12",
	"transithub-thinking-signature-13",
	"transithub-thinking-signature-14",
	"transithub-thinking-signature-15",
	"transithub-thinking-signature-16",
	"transithub-thinking-signature-17",
	"transithub-thinking-signature-18",
	"transithub-thinking-signature-19",
	"transithub-thinking-signature-20",
	"transithub-thinking-signature-21",
	"transithub-thinking-signature-22",
	"transithub-thinking-signature-23",
	"transithub-thinking-signature-24",
	"transithub-thinking-signature-25",
	"transithub-thinking-signature-26",
	"transithub-thinking-signature-27",
	"transithub-thinking-signature-28",
	"transithub-thinking-signature-29",
	"transithub-thinking-signature-30",
}

// Each thinking block samples independently; repeated labels are allowed.
func pickMappedThinkingSignature() string {
	return reservedThinkingSignatures[rand.IntN(len(reservedThinkingSignatures))]
}
