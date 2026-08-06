package updater

import (
	"strings"
	"testing"
)

func TestResolveHeaderMacros(t *testing.T) {
	header := strings.Join([]string{
		`#define RCC_CFGR_SWS_Pos (3U)`,
		`#define RCC_CFGR_SWS_Msk (0x7UL << RCC_CFGR_SWS_Pos) /*!< 0x00000038 */`,
		`#define RCC_CFGR_SWS RCC_CFGR_SWS_Msk`,
		`#define RCC_CFGR_SWS_HSI (0UL) /*!< HSI used as system clock */`,
		`#define RCC_CFGR_SWS_HSE ((uint32_t)(0x1UL << RCC_CFGR_SWS_Pos))`,
		`#define RCC_CFGR_SWS_PLL (RCC_CFGR_SWS_HSE | (0x1UL << 4))`,
		`#define RCC_CFGR_SWS_LSI (UINT32_C(3) << RCC_CFGR_SWS_Pos)`,
		`#define ADC_CFGR1_RESSEL_0 (0x1UL << 3)`,
		`#define ADC_CFGR1_RESSEL_1 (0x2UL << 3)`,
		`#define MULTILINE ((1UL << 2) \`,
		`                   | (1UL << 4))`,
		`#define FUNCTION(x) ((x) << 1)`,
		`#define UNSUPPORTED (sizeof(uint32_t))`,
		`#define CYCLE_A CYCLE_B`,
		`#define CYCLE_B CYCLE_A`,
	}, "\r\n")

	macros := resolveHeaderMacros([]headerSource{{Path: "device.h", Data: []byte(header)}})
	checks := map[string]struct {
		value       uint64
		description string
	}{
		"RCC_CFGR_SWS_POS": {3, ""},
		"RCC_CFGR_SWS_MSK": {0x38, ""},
		"RCC_CFGR_SWS":     {0x38, ""},
		"RCC_CFGR_SWS_HSI": {0, "HSI used as system clock"},
		"RCC_CFGR_SWS_HSE": {8, ""},
		"RCC_CFGR_SWS_PLL": {24, ""},
		"RCC_CFGR_SWS_LSI": {24, ""},
		"MULTILINE":        {20, ""},
	}
	for name, want := range checks {
		got, ok := macros[name]
		if !ok {
			t.Errorf("macro %s was not resolved", name)
			continue
		}
		if got.Value != want.value || got.Description != want.description {
			t.Errorf("macro %s = (%#x, %q), want (%#x, %q)", name, got.Value, got.Description, want.value, want.description)
		}
	}
	for _, name := range []string{"FUNCTION", "UNSUPPORTED", "CYCLE_A", "CYCLE_B"} {
		if _, ok := macros[name]; ok {
			t.Errorf("unsupported macro %s was resolved", name)
		}
	}
}

func TestResolveHeaderMacrosConflicts(t *testing.T) {
	headers := []headerSource{
		{Path: "a.h", Data: []byte(`#define RCC_CFGR_SWS_HSI 0`)},
		{Path: "b.h", Data: []byte(`#define RCC_CFGR_SWS_HSI 8`)},
	}
	if _, ok := resolveHeaderMacros(headers)["RCC_CFGR_SWS_HSI"]; ok {
		t.Fatal("conflicting macro was returned")
	}
}

func TestResolveHeaderMacrosRequiresEveryHeader(t *testing.T) {
	headers := []headerSource{
		{Path: "a.h", Data: []byte("#define RCC_CFGR_SWS_HSI 0\n#define RCC_CFGR_SWS_HSE 1")},
		{Path: "b.h", Data: []byte("#define RCC_CFGR_SWS_HSI 0")},
	}
	macros := resolveHeaderMacros(headers)
	if _, ok := macros["RCC_CFGR_SWS_HSE"]; ok {
		t.Fatal("variant-specific macro was returned")
	}
	if got, ok := macros["RCC_CFGR_SWS_HSI"]; !ok || got.Value != 0 {
		t.Fatalf("common macro = (%#v, %v), want value 0", got, ok)
	}
}

func TestResolveHeaderMacrosConditionalDefinitions(t *testing.T) {
	header := []byte(strings.Join([]string{
		`#if DEVICE_A`,
		`#define RCC_CFGR_SWS_HSI 0`,
		`#else`,
		`#define RCC_CFGR_SWS_HSI 8`,
		`#endif`,
		`#define RCC_CFGR_SWS_HSE 16`,
	}, "\n"))
	macros := resolveHeaderMacros([]headerSource{{Path: "device.h", Data: header}})
	if _, ok := macros["RCC_CFGR_SWS_HSI"]; ok {
		t.Fatal("conditionally conflicting macro was returned")
	}
	if got := macros["RCC_CFGR_SWS_HSE"].Value; got != 16 {
		t.Fatalf("unambiguous macro = %d, want 16", got)
	}
}

func TestResolveHeaderMacrosRejectsUnsupportedAlternative(t *testing.T) {
	header := []byte(strings.Join([]string{
		`#if DEVICE_A`,
		`#define RCC_CFGR_SWS_HSI FUNCTION(0)`,
		`#else`,
		`#define RCC_CFGR_SWS_HSI 0`,
		`#endif`,
	}, "\n"))
	if _, ok := resolveHeaderMacros([]headerSource{{Path: "device.h", Data: header}})["RCC_CFGR_SWS_HSI"]; ok {
		t.Fatal("macro with an unsupported conditional alternative was returned")
	}
}

func TestResolveHeaderMacrosDescriptionConsensus(t *testing.T) {
	headers := []headerSource{
		{Path: "a.h", Data: []byte(`#define RCC_CFGR_SWS_HSI 0 /*!< first description */`)},
		{Path: "b.h", Data: []byte(`#define RCC_CFGR_SWS_HSI 0 /*!< different description */`)},
	}
	got, ok := resolveHeaderMacros(headers)["RCC_CFGR_SWS_HSI"]
	if !ok {
		t.Fatal("common macro was not returned")
	}
	if got.Description != "" {
		t.Fatalf("conflicting description = %q, want empty", got.Description)
	}
}

func TestEvaluateHeaderExpressionRejectsOverflow(t *testing.T) {
	for _, expression := range []string{
		`0xffffffffffffffffUL + 1`,
		`2UL << 63`,
		`1UL << 64`,
		`1UL / 0`,
		`0UL - 1`,
	} {
		if value, ok := evaluateHeaderExpression(expression, func(string) (uint64, bool) { return 0, false }); ok {
			t.Errorf("expression %q unexpectedly evaluated to %#x", expression, value)
		}
	}
}
