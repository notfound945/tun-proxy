package defaultconfig

import (
	"bytes"
	"os"
	"testing"
)

func TestEmbeddedConfigMatchesRepositoryExample(t *testing.T) {
	repositoryExample, err := os.ReadFile("../../configs/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(Bytes(), repositoryExample) {
		t.Fatal("embedded default config and configs/config.yaml differ")
	}
}
