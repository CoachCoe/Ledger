package canon

import (
	"bufio"
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRFC8785Vectors(t *testing.T) {
	inputs, err := filepath.Glob("testdata/rfc8785/input/*.json")
	if err != nil || len(inputs) == 0 {
		t.Fatalf("no vectors: %v", err)
	}
	for _, in := range inputs {
		raw, err := os.ReadFile(in)
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(filepath.Join("testdata/rfc8785/output", filepath.Base(in)))
		if err != nil {
			t.Fatal(err)
		}
		v := jsontext.Value(raw)
		if err := v.Canonicalize(); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if !bytes.Equal(v, bytes.TrimRight(want, "\n")) {
			t.Errorf("%s:\n got %s\nwant %s", in, v, want)
		}
	}
}

func TestJavaScriptReference(t *testing.T) {
	f, err := os.Open("testdata/js-reference.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	n := 0
	for sc.Scan() {
		var c struct {
			Input     string `json:"input"`
			Canonical string `json:"canonical"`
		}
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		got, err := Canonical([]byte(c.Input))
		if err != nil {
			t.Fatalf("line %d: %v", n+1, err)
		}
		if string(got) != c.Canonical {
			t.Fatalf("line %d:\n got %s\nwant %s", n+1, got, c.Canonical)
		}
		n++
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if n != 400 {
		t.Fatalf("read %d fixtures, want 400", n)
	}
}

func TestPolicyAccepts(t *testing.T) {
	cases := map[string]string{
		`{"b":1,"a":2}`:                       `{"a":2,"b":1}`,
		`9007199254740991`:                    `9007199254740991`,
		`-9007199254740991`:                   `-9007199254740991`,
		`0`:                                   `0`,
		`{"\uE000":1,"\uD83D\uDE00":2}`:       "{\"\U0001F600\":2,\"\uE000\":1}",
		`"\u00e9"`:                            `"é"`,
		"\"e\u0301\"":                         "\"e\u0301\"",
		`"\u001f\b\f\n\r\t\"\\/"`:             `"\u001f\b\f\n\r\t\"\\/"`,
		`"\u2028\u2029"`:                      "\"\u2028\u2029\"",
		`{ "x" : [ 1 , true , null , "y" ] }`: `{"x":[1,true,null,"y"]}`,
		`"<>&"`:                               `"<>&"`,
		`"\uD83D\uDE00"`:                      `"😀"`,
	}
	for in, want := range cases {
		got, err := Canonical([]byte(in))
		if err != nil {
			t.Errorf("Canonical(%s): %v", in, err)
		} else if string(got) != want {
			t.Errorf("Canonical(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestPolicyRejects(t *testing.T) {
	for _, in := range []string{
		`{"a":1,"a":2}`, `{"a":1,"\u0061":2}`,
		`1.0`, `1e2`, `1E2`, `-0`, `0.5`, `01`, `-01`,
		`9007199254740992`, `-9007199254740992`, `123456789012345678901234567890`,
		`"\ud800"`, `"\udc00\ud800"`, `"\ud83d"`,
		"\"\xff\"", "\"\xc3\x28\"", "\xef\xbb\xbf{}",
		`{} {}`, `{}x`, `[1,]`, `{"a":1,}`, `NaN`, `Infinity`, ``,
	} {
		if got, err := Canonical([]byte(in)); err == nil {
			t.Errorf("Canonical(%q) = %s, want error", in, got)
		}
	}
	if _, err := Canonical([]byte(`1.5`)); !errors.Is(err, ErrNumber) {
		t.Errorf("fraction error = %v, want ErrNumber", err)
	}
	if _, err := Canonical([]byte(`{}{}`)); !errors.Is(err, ErrTrailing) {
		t.Errorf("trailing error = %v, want ErrTrailing", err)
	}
}

func TestHashIgnoresKeyOrderAndWhitespace(t *testing.T) {
	a, err := Canonical([]byte(`{"z":{"b":[3,2,1],"a":"x"},"y":null}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Canonical([]byte("{\n  \"y\" : null,\n  \"z\" : { \"a\":\"x\", \"b\":[3,2,1] }\n}"))
	if err != nil {
		t.Fatal(err)
	}
	if Hash(a) != Hash(b) {
		t.Fatalf("hashes differ: %s vs %s", a, b)
	}
	if got := Hash([]byte("abc")); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("Hash(abc) = %s", got)
	}
}

func TestMarshal(t *testing.T) {
	got, err := Marshal(struct {
		B int    `json:"b"`
		A string `json:"a"`
	}{1, "<"})
	if err != nil || string(got) != `{"a":"<","b":1}` {
		t.Fatalf("Marshal = %s, %v", got, err)
	}
	if _, err := Marshal(map[string]float64{"x": 1.5}); err == nil {
		t.Fatal("Marshal accepted a fraction")
	}
	if _, err := Marshal(map[string]int64{"x": MaxSafe + 1}); err == nil {
		t.Fatal("Marshal accepted an unsafe integer")
	}
}

func FuzzCanonical(f *testing.F) {
	for _, s := range []string{`{"a":1}`, `[1,"x",{"b":null}]`, `"\ud83d\ude00"`, `{"\u00e9":true}`, `-12`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		out, err := Canonical(raw)
		if err != nil {
			return
		}
		again, err := Canonical(out)
		if err != nil {
			t.Fatalf("canonical output rejected: %s: %v", out, err)
		}
		if !bytes.Equal(out, again) {
			t.Fatalf("not idempotent: %s -> %s", out, again)
		}
		var x, y any
		if json.Unmarshal(raw, &x) != nil || json.Unmarshal(out, &y) != nil {
			t.Fatalf("decode failed on accepted input %q", raw)
		}
		if !reflect.DeepEqual(x, y) {
			t.Fatalf("meaning changed: %q -> %s", raw, out)
		}
	})
}
