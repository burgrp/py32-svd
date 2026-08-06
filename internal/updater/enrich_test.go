package updater

import (
	"bytes"
	"encoding/xml"
	"strings"
	"testing"
)

func TestEnrichSVDFromHeaders(t *testing.T) {
	input := strings.Join([]string{
		`<?xml version="1.0"?>`,
		`<device>`,
		`  <name>PY32TEST</name>`,
		`  <peripherals>`,
		`    <peripheral>`,
		`      <name>RCC</name>`,
		`      <groupName>RCC</groupName>`,
		`      <registers>`,
		`        <register>`,
		`          <name>CFGR</name>`,
		`          <fields>`,
		`            <field>`,
		`              <name>SWS</name>`,
		`              <bitRange>[5:3]</bitRange>`,
		`            </field>`,
		`            <field>`,
		`              <name>KEEP</name>`,
		`              <bitOffset>8</bitOffset>`,
		`              <bitWidth>2</bitWidth>`,
		`              <enumeratedValues><enumeratedValue><name>OLD</name><value>1</value></enumeratedValue></enumeratedValues>`,
		`            </field>`,
		`          </fields>`,
		`        </register>`,
		`      </registers>`,
		`    </peripheral>`,
		`  </peripherals>`,
		`</device>`,
	}, "\r\n")
	header := strings.Join([]string{
		`#define RCC_CFGR_SWS_Pos (3U)`,
		`#define RCC_CFGR_SWS_Msk (0x7UL << RCC_CFGR_SWS_Pos)`,
		`#define RCC_CFGR_SWS_HSI (0UL) /*!< HSI used as system clock */`,
		`#define RCC_CFGR_SWS_HSE (1UL << RCC_CFGR_SWS_Pos) /*!< HSE & crystal */`,
		`#define RCC_CFGR_SWS_PLL (2UL << RCC_CFGR_SWS_Pos)`,
		`#define RCC_CFGR_SWS_0 (1UL << RCC_CFGR_SWS_Pos)`,
		`#define RCC_CFGR_SWS_BAD (1UL << 31)`,
		`#define RCC_CFGR_KEEP_NEW (2UL << 8)`,
	}, "\n")

	got, count, err := enrichSVDFromHeaders([]byte(input), []headerSource{{Path: "device.h", Data: []byte(header)}})
	if err != nil {
		t.Fatal(err)
	}
	if count != 4 {
		t.Fatalf("inserted value count = %d, want 4", count)
	}
	if bytes.Contains(got, []byte("<name>BAD</name>")) {
		t.Fatal("out-of-field value was inserted")
	}
	if bytes.Contains(got, []byte("<name>NEW</name>")) {
		t.Fatal("field with existing enumerations was modified")
	}
	if !bytes.Contains(got, []byte("HSE &amp; crystal")) {
		t.Fatal("enumeration description was not XML escaped")
	}
	if bytes.Contains(got, []byte("\n")) && !bytes.Contains(got, []byte("\r\n")) {
		t.Fatal("CRLF line endings were not preserved")
	}
	if count := bytes.Count(got, []byte("<enumeratedValues>")); count != 2 {
		t.Fatalf("enumeratedValues count = %d, want 2 (one existing, one added)\n%s", count, got)
	}

	var device any
	if err := xml.Unmarshal(got, &device); err != nil {
		t.Fatalf("enriched SVD is invalid XML: %v", err)
	}
	for _, fragment := range []string{
		`<name>HSI</name>`, `<value>0</value>`,
		`<name>HSE</name>`, `<value>1</value>`,
		`<name>0</name>`, `<name>PLL</name>`, `<value>2</value>`,
	} {
		if !bytes.Contains(got, []byte(fragment)) {
			t.Errorf("enriched SVD does not contain %q", fragment)
		}
	}

	again, secondCount, err := enrichSVDFromHeaders(got, []headerSource{{Path: "device.h", Data: []byte(header)}})
	if err != nil {
		t.Fatal(err)
	}
	if secondCount != 0 || !bytes.Equal(got, again) {
		t.Fatal("header enrichment is not idempotent")
	}
}

func TestEnrichSVDUsesGroupNameAndFieldForms(t *testing.T) {
	input := `<device><name>PY32TEST</name><peripherals><peripheral><name>USART1</name><groupName>USART</groupName><registers><register><name>CR1</name><fields>` +
		`<field><name>MODE</name><lsb>4</lsb><msb>5</msb></field>` +
		`<field derivedFrom="MODE"><name>COPY</name><bitRange>[7:6]</bitRange></field>` +
		`</fields></register></registers></peripheral></peripherals></device>`
	header := strings.Join([]string{
		`#define USART_CR1_MODE_Pos 4U`,
		`#define USART_CR1_MODE_Msk (3U << USART_CR1_MODE_Pos)`,
		`#define USART_CR1_MODE_SYNC (1U << USART_CR1_MODE_Pos)`,
	}, "\n")
	got, count, err := enrichSVDFromHeaders([]byte(input), []headerSource{{Path: "device.h", Data: []byte(header)}})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || !bytes.Contains(got, []byte(`<name>SYNC</name><value>1</value>`)) {
		t.Fatalf("group-name enum was not added: count=%d, output=%s", count, got)
	}
	if bytes.Count(got, []byte("<enumeratedValues>")) != 1 {
		t.Fatal("derived field was enriched")
	}
}

func TestEnrichSVDRejectsHeaderDisagreement(t *testing.T) {
	input := `<device><name>PY32TEST</name><peripherals><peripheral><name>RCC</name><registers><register><name>CFGR</name><fields><field><name>SWS</name><bitRange>[1:0]</bitRange></field></fields></register></registers></peripheral></peripherals></device>`
	headers := []headerSource{
		{Path: "a.h", Data: []byte("#define RCC_CFGR_SWS_Pos 0\n#define RCC_CFGR_SWS_Msk 3\n#define RCC_CFGR_SWS_HSI 0")},
		{Path: "b.h", Data: []byte("#define RCC_CFGR_SWS_Pos 0\n#define RCC_CFGR_SWS_Msk 3\n#define RCC_CFGR_SWS_HSI 1")},
	}
	got, count, err := enrichSVDFromHeaders([]byte(input), headers)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 || !bytes.Equal(got, []byte(input)) {
		t.Fatalf("conflicting header value was added: %s", got)
	}
}

func TestEnrichSVDRejectsFieldLayoutMismatch(t *testing.T) {
	input := `<device><name>PY32TEST</name><peripherals><peripheral><name>RCC</name><registers><register><name>CFGR</name><fields><field><name>SWS</name><bitRange>[5:3]</bitRange></field></fields></register></registers></peripheral></peripherals></device>`
	header := strings.Join([]string{
		`#define RCC_CFGR_SWS_Pos 4`,
		`#define RCC_CFGR_SWS_Msk (7U << RCC_CFGR_SWS_Pos)`,
		`#define RCC_CFGR_SWS_HSI 0`,
	}, "\n")
	got, count, err := enrichSVDFromHeaders([]byte(input), []headerSource{{Path: "device.h", Data: []byte(header)}})
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 || !bytes.Equal(got, []byte(input)) {
		t.Fatalf("value from mismatched field layout was added: %s", got)
	}
}

func TestAssociateDeviceHeaders(t *testing.T) {
	svds := []archiveSource{
		{path: "CMSIS/SVD/PY32F040xx.svd"},
		{path: "CMSIS/SVD/PY32F040Cxx.svd"},
		{path: "CMSIS/SVD/PY32F040Exx.svd"},
		{path: "CMSIS/SVD/PY32F040EPxx.svd"},
	}
	headers := []headerSource{
		{Path: "Drivers/CMSIS/Device/PY32F0xx/Include/py32f040x6.h"},
		{Path: "Drivers/CMSIS/Device/PY32F0xx/Include/py32f040cx6.h"},
		{Path: "Drivers/CMSIS/Device/PY32F0xx/Include/py32f040ex6.h"},
		{Path: "Drivers/CMSIS/Device/PY32F0xx/Include/py32f040epxB.h"},
		{Path: "Drivers/CMSIS/Device/PY32F0xx/Include/py32f0xx.h"},
	}
	got := associateDeviceHeaders(svds, headers)
	checks := map[string]string{
		"CMSIS/SVD/PY32F040xx.svd":   "py32f040x6.h",
		"CMSIS/SVD/PY32F040Cxx.svd":  "py32f040cx6.h",
		"CMSIS/SVD/PY32F040Exx.svd":  "py32f040ex6.h",
		"CMSIS/SVD/PY32F040EPxx.svd": "py32f040epxB.h",
	}
	for svdPath, headerBase := range checks {
		associated := got[svdPath]
		if len(associated) != 1 || !strings.HasSuffix(associated[0].Path, headerBase) {
			t.Errorf("headers for %s = %#v, want %s", svdPath, associated, headerBase)
		}
	}
}
