package pull

import (
	"errors"
	"fmt"
	"strings"
)

// Sources selects where a pull may fetch content from.
//
// The two are ordered, not exclusive: with both set the swarm is tried first and
// Hugging Face only gets the request when the p2p path cannot deliver. That is
// the default because it keeps HF bandwidth down while never turning a missing
// seeder into a failed download.
type Sources struct {
	P2P bool
	HF  bool
}

// DefaultSources tries the swarm first, then falls back to Hugging Face.
func DefaultSources() Sources { return Sources{P2P: true, HF: true} }

// ParseSources parses a `--from` spec: "p2p", "hf", or "p2p,hf".
//
// Aliases are accepted because people reach for different words for the same
// thing; anything else is an error rather than a silent default, so a typo
// cannot quietly change where a multi-gigabyte download comes from.
func ParseSources(spec string) (Sources, error) {
	if strings.TrimSpace(spec) == "" {
		return DefaultSources(), nil
	}
	var s Sources
	for _, tok := range strings.Split(spec, ",") {
		switch strings.ToLower(strings.TrimSpace(tok)) {
		case "p2p", "ipfs", "swarm", "peer":
			s.P2P = true
		case "hf", "huggingface", "hugging-face", "hub":
			s.HF = true
		case "":
			continue
		default:
			return Sources{}, fmt.Errorf("unknown source %q: want p2p, hf, or p2p,hf", tok)
		}
	}
	if !s.P2P && !s.HF {
		return Sources{}, errors.New("at least one of p2p, hf must be selected")
	}
	return s, nil
}

// String renders the canonical spec form, which is what goes on the wire to the
// daemon so both sides agree on one spelling.
func (s Sources) String() string {
	parts := make([]string, 0, 2)
	if s.P2P {
		parts = append(parts, "p2p")
	}
	if s.HF {
		parts = append(parts, "hf")
	}
	return strings.Join(parts, ",")
}

// resolve treats the zero value as "unset" so a pull built without an explicit
// source list still gets the default behaviour.
func (s Sources) resolve() Sources {
	if s.P2P || s.HF {
		return s
	}
	return DefaultSources()
}
