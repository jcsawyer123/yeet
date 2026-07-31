package coolify

import (
	"reflect"
	"testing"
)

func TestParseEnvBlob(t *testing.T) {
	blob := "" +
		"API_KEY=sk-abc123\n" +
		"\n" +
		"# a comment\n" +
		"  DB_URL = postgres://user:pass@host/db?opt=1  \n" +
		"EMPTY_VALUE=\n" +
		"  \n" +
		"NOEQUALS\n" +
		"=nokey\n"

	got := ParseEnvBlob(blob)
	want := []EnvVar{
		{Key: "API_KEY", Value: "sk-abc123"},
		{Key: "DB_URL", Value: " postgres://user:pass@host/db?opt=1"},
		{Key: "EMPTY_VALUE", Value: ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestParseEnvBlobEmpty(t *testing.T) {
	if got := ParseEnvBlob(""); got != nil {
		t.Fatalf("expected nil for empty blob, got %+v", got)
	}
}
