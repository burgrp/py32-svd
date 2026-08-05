package updater

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestNormalizeGPIOGroupNames(t *testing.T) {
	input := strings.Join([]string{
		`<?xml version="1.0"?>`,
		`<device>`,
		`  <name>PY32TEST</name>`,
		`  <peripherals>`,
		`    <peripheral>`,
		`      <name>GPIOA</name>`,
		`      <groupName>GPIOA</groupName>`,
		`      <baseAddress>0x50000000</baseAddress>`,
		`      <registers><register><name>MODER</name><addressOffset>0</addressOffset><size>32</size></register></registers>`,
		`    </peripheral>`,
		`    <peripheral derivedFrom="GPIOA">`,
		`      <name>GPIOB</name>`,
		`      <baseAddress>0x50000400</baseAddress>`,
		`    </peripheral>`,
		`    <peripheral>`,
		`      <name>GPIOC</name>`,
		`      <baseAddress>0x50000800</baseAddress>`,
		`      <registers><register><name>MODER</name><addressOffset>0</addressOffset><size>32</size></register></registers>`,
		`    </peripheral>`,
		`    <peripheral><name>USART1</name><groupName>USART</groupName></peripheral>`,
		`  </peripherals>`,
		`</device>`,
	}, "\r\n")

	got, err := normalizeGPIOGroupNames([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if count := bytes.Count(got, []byte("<groupName>GPIO</groupName>")); count != 3 {
		t.Fatalf("got %d normalized GPIO group names, want 3\n%s", count, got)
	}
	if !bytes.Contains(got, []byte("<groupName>USART</groupName>")) {
		t.Fatal("non-GPIO peripheral was modified")
	}
	if bytes.Contains(got, []byte("\n")) && !bytes.Contains(got, []byte("\r\n")) {
		t.Fatal("CRLF line endings were not preserved")
	}

	again, err := normalizeGPIOGroupNames(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, again) {
		t.Fatal("GPIO normalization is not idempotent")
	}
}

func TestNormalizeGPIOGroupNamesRejectsDifferentLayouts(t *testing.T) {
	input := `<device><name>PY32TEST</name><peripherals>` +
		`<peripheral><name>GPIOA</name><registers><register><name>MODER</name><addressOffset>0</addressOffset></register></registers></peripheral>` +
		`<peripheral><name>GPIOB</name><registers><register><name>MODER</name><addressOffset>4</addressOffset></register></registers></peripheral>` +
		`</peripherals></device>`

	_, err := normalizeGPIOGroupNames([]byte(input))
	if err == nil || !strings.Contains(err.Error(), "conflicting register layouts") {
		t.Fatalf("error = %v, want conflicting register layouts", err)
	}
}

func TestNormalizeGPIOGroupNamesAllowsPartialLayout(t *testing.T) {
	input := `<device><name>PY32TEST</name><peripherals>` +
		`<peripheral><name>GPIOA</name><registers><register><name>MODER</name><addressOffset>0</addressOffset></register><register><name>AFRH</name><addressOffset>0x24</addressOffset></register></registers></peripheral>` +
		`<peripheral><name>GPIOF</name><registers><register><name>MODER</name><addressOffset>0</addressOffset></register></registers></peripheral>` +
		`</peripherals></device>`

	got, err := normalizeGPIOGroupNames([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if count := bytes.Count(got, []byte("<groupName>GPIO</groupName>")); count != 2 {
		t.Fatalf("got %d normalized GPIO group names, want 2", count)
	}
}

func TestNormalizeGPIOGroupNamesLeavesUnrelatedSVDUnchanged(t *testing.T) {
	input := []byte(`<?xml version="1.0"?><device><name>PY32TEST</name><peripherals><peripheral><name>USART1</name></peripheral></peripherals></device>`)
	got, err := normalizeGPIOGroupNames(input)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, input) {
		t.Fatalf("unrelated SVD changed: %s", got)
	}
}

func TestExtractPublishesAndHashesPatchedSVD(t *testing.T) {
	input := `<device><name>PY32TEST</name><peripherals>` +
		`<peripheral><name>GPIOA</name><registers><register><name>MODER</name><addressOffset>0</addressOffset></register></registers></peripheral>` +
		`<peripheral derivedFrom="GPIOA"><name>GPIOB</name></peripheral>` +
		`</peripherals></device>`

	files, err := extract("test", makePack(t, zipEntry{name: "test.svd", data: input}), 4096, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("got %d files, want 1", len(files))
	}
	if bytes.Equal(files[0].data, []byte(input)) {
		t.Fatal("extract published the unpatched SVD")
	}
	if count := bytes.Count(files[0].data, []byte("<groupName>GPIO</groupName>")); count != 2 {
		t.Fatalf("got %d normalized GPIO group names, want 2", count)
	}
	sum := sha256.Sum256(files[0].data)
	if got, want := files[0].manifest.SHA256, hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("manifest hash = %s, want patched-data hash %s", got, want)
	}
}
