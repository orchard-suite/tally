package agents

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"
)

func TestAnEnrolledMachinePostsUnderItsName(t *testing.T) {
	c := testCollector(t, "secret")
	if code := post(c, "Bearer fleet", batch); code != http.StatusUnauthorized {
		t.Fatalf("not yet enrolled: %d", code)
	}
	sum := sha256.Sum256([]byte("fleet"))
	mine := sha256.Sum256([]byte("secret"))
	c.Enroll(map[string]string{hex.EncodeToString(sum[:]): "build-box", hex.EncodeToString(mine[:]): "someone-else"})
	if code := post(c, "Bearer fleet", batch); code != http.StatusNoContent {
		t.Fatalf("enrolled: %d", code)
	}
	req, _ := http.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Authorization", "Bearer secret")
	if m := c.Machine(req); m != "laptop" {
		t.Errorf("a machine SHIP_TOKENS names keeps its name: %q", m)
	}
	c.Enroll(map[string]string{})
	if code := post(c, "Bearer fleet", batch); code != http.StatusUnauthorized {
		t.Fatalf("a machine Intune no longer names is refused: %d", code)
	}
}
