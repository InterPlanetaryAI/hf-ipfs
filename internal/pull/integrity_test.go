package pull

import (
	"strings"
	"testing"

	"github.com/ipai/hf-ipfs/internal/dummy"
	"github.com/ipai/hf-ipfs/internal/wire"
)

func TestValidateMapBinding(t *testing.T) {
	want, err := dummy.FromCommit(testCommit)
	if err != nil {
		t.Fatal(err)
	}
	ok := &wire.MapResponse{CommitHash: testCommit, DummyCID: want.String()}
	if err := validateMapBinding(testCommit, ok); err != nil {
		t.Errorf("honest response rejected: %v", err)
	}
	if err := validateMapBinding(testCommit, &wire.MapResponse{CommitHash: "other"}); err == nil {
		t.Error("mismatched commit hash accepted")
	}
	if err := validateMapBinding(testCommit, &wire.MapResponse{CommitHash: testCommit, DummyCID: "bafygarbage"}); err == nil {
		t.Error("unparsable dummy cid accepted")
	}
	other, _ := dummy.FromCommit(strings.Repeat("f", 40))
	if err := validateMapBinding(testCommit, &wire.MapResponse{CommitHash: testCommit, DummyCID: other.String()}); err == nil {
		t.Error("foreign dummy cid accepted")
	}
	if err := validateMapBinding(testCommit, &wire.MapResponse{CommitHash: testCommit}); err != nil {
		t.Errorf("absent dummy cid rejected: %v", err)
	}
}
