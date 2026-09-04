package pull

import "testing"

func TestParseSources(t *testing.T) {
	for _, tc := range []struct {
		spec string
		want Sources
	}{
		{"p2p", Sources{P2P: true}},
		{"hf", Sources{HF: true}},
		{"p2p,hf", Sources{P2P: true, HF: true}},
		{"hf,p2p", Sources{P2P: true, HF: true}},
		{" p2p , hf ", Sources{P2P: true, HF: true}},
		{"P2P,HF", Sources{P2P: true, HF: true}},
		{"p2p,p2p", Sources{P2P: true}},
		{"", DefaultSources()},
		{"ipfs", Sources{P2P: true}},
		{"swarm", Sources{P2P: true}},
		{"huggingface", Sources{HF: true}},
		{"hub", Sources{HF: true}},
	} {
		got, err := ParseSources(tc.spec)
		if err != nil {
			t.Errorf("ParseSources(%q): unexpected error %v", tc.spec, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseSources(%q) = %+v, want %+v", tc.spec, got, tc.want)
		}
	}
}

func TestParseSourcesRejectsGarbage(t *testing.T) {
	// A typo must not silently change where a multi-gigabyte download comes
	// from, so every unrecognised token is an error.
	for _, spec := range []string{"bogus", "p2p,bogus", "hf,http", ","} {
		if _, err := ParseSources(spec); err == nil {
			t.Errorf("ParseSources(%q): want error, got nil", spec)
		}
	}
}

func TestSourcesResolveZeroValueIsDefault(t *testing.T) {
	// Options built without an explicit Sources must keep the shipped
	// behaviour rather than silently disabling both paths.
	if got := (Sources{}).resolve(); got != DefaultSources() {
		t.Errorf("Sources{}.resolve() = %+v, want %+v", got, DefaultSources())
	}
	if got := (Sources{HF: true}).resolve(); got != (Sources{HF: true}) {
		t.Errorf("explicit sources were widened: %+v", got)
	}
}

func TestSourcesStringRoundTrips(t *testing.T) {
	for _, want := range []Sources{
		{P2P: true},
		{HF: true},
		{P2P: true, HF: true},
	} {
		got, err := ParseSources(want.String())
		if err != nil {
			t.Fatalf("ParseSources(%q): %v", want.String(), err)
		}
		if got != want {
			t.Errorf("round trip: %v -> %q -> %v", want, want.String(), got)
		}
	}
}
