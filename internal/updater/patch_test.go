package updater

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"reflect"
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

func TestAddHSIFrequencyValues(t *testing.T) {
	input := `<device><name>PY32TEST</name><peripherals><peripheral><name>RCC</name><registers><register><name>ICSCR</name><fields><field><name>HSI_FS</name><bitRange>[15:13]</bitRange></field></fields></register></registers></peripheral></peripherals></device>`
	header := strings.Join([]string{
		`#define RCC_ICSCR_HSI_FS_Pos 13U`,
		`#define RCC_ICSCR_HSI_FS_Msk (7U << RCC_ICSCR_HSI_FS_Pos)`,
		`#define RCC_ICSCR_HSI_FS_0 (1U << RCC_ICSCR_HSI_FS_Pos)`,
		`#define RCC_ICSCR_HSI_FS_1 (2U << RCC_ICSCR_HSI_FS_Pos)`,
		`#define RCC_ICSCR_HSI_FS_2 (4U << RCC_ICSCR_HSI_FS_Pos)`,
	}, "\n")

	patched, err := addHSIFrequencyValues([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	got, count, err := enrichSVDFromHeaders(patched, []headerSource{{Path: "device.h", Data: []byte(header)}})
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("header enrichment added %d HSI_FS bit components, want 0", count)
	}
	if !bytes.Contains(got, []byte(`<name>Freq24MHz</name><description>24 MHz HSI clock</description><value>4</value>`)) {
		t.Fatalf("semantic HSI_FS value was not added: %s", got)
	}
	if bytes.Contains(got, []byte(`<name>2</name>`)) {
		t.Fatalf("HSI_FS bit component was published as an enumeration: %s", got)
	}

	again, err := addHSIFrequencyValues(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, again) {
		t.Fatal("HSI frequency patch is not idempotent")
	}

	f032 := strings.Replace(input, "PY32TEST", "PY32F032xx", 1)
	got, err = addHSIFrequencyValues([]byte(f032))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte(`<name>Freq24MHz</name><description>24 MHz HSI clock</description><value>3</value>`)) {
		t.Fatalf("F032 semantic HSI_FS value was not added with encoding 3: %s", got)
	}
}

func TestAddHSIFrequencyValuesLeavesOtherLayoutsUnchanged(t *testing.T) {
	input := []byte(`<device><name>PY32TEST</name><peripherals><peripheral><name>RCC</name><registers><register><name>ICSCR</name><fields><field><name>HSI_FS</name><bitRange>[17:16]</bitRange></field></fields></register></registers></peripheral></peripherals></device>`)
	got, err := addHSIFrequencyValues(input)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, input) {
		t.Fatalf("non-three-bit HSI_FS field changed: %s", got)
	}
}

func TestExtractPublishesAndHashesPatchedSVD(t *testing.T) {
	input := `<device><name>PY32TEST</name><peripherals>` +
		`<peripheral><name>GPIOA</name><registers><register><name>MODER</name><addressOffset>0</addressOffset><fields><field><name>MODE0</name><bitRange>[1:0]</bitRange></field></fields></register></registers></peripheral>` +
		`<peripheral derivedFrom="GPIOA"><name>GPIOB</name></peripheral>` +
		`</peripherals></device>`
	headerPath := "Drivers/CMSIS/Device/PY32TEST/Include/py32testx1.h"
	header := "#define GPIO_MODER_MODE0_Pos 0U\n" +
		"#define GPIO_MODER_MODE0_Msk (3U << GPIO_MODER_MODE0_Pos)\n" +
		"#define GPIO_MODER_MODE0_OUTPUT (1U << GPIO_MODER_MODE0_Pos)"

	files, err := extract("test", makePack(t,
		zipEntry{name: "py32testxx.svd", data: input},
		zipEntry{name: headerPath, data: header},
	), 4096, 8192)
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
	if !bytes.Contains(files[0].data, []byte("<name>OUTPUT</name>")) {
		t.Fatal("extract did not publish the header-enriched SVD")
	}
	if got, want := files[0].manifest.Headers, []string{headerPath}; !reflect.DeepEqual(got, want) {
		t.Fatalf("manifest headers = %v, want %v", got, want)
	}
	if got := files[0].manifest.HeaderEnumeratedValues; got != 1 {
		t.Fatalf("manifest header enumeration count = %d, want 1", got)
	}
	sum := sha256.Sum256(files[0].data)
	if got, want := files[0].manifest.SHA256, hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("manifest hash = %s, want patched-data hash %s", got, want)
	}
}
