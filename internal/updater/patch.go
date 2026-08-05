package updater

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"regexp"
	"strings"
)

var peripheralElementRE = regexp.MustCompile(`(?s)<peripheral(?:\s[^>]*)?>.*?</peripheral>`)
var groupNameElementRE = regexp.MustCompile(`(?s)(<groupName(?:\s[^>]*)?>)[^<]*(</groupName>)`)

type patchPeripheral struct {
	Name        string          `xml:"name"`
	GroupName   string          `xml:"groupName"`
	DerivedFrom string          `xml:"derivedFrom,attr"`
	Registers   *registerLayout `xml:"registers"`
}

type registerLayout struct {
	Registers []registerLayoutEntry `xml:"register"`
	Clusters  []registerLayoutEntry `xml:"cluster"`
}

type registerLayoutEntry struct {
	Name          string                `xml:"name"`
	Offset        string                `xml:"offset"`
	AddressOffset string                `xml:"addressOffset"`
	Size          string                `xml:"size"`
	Dim           string                `xml:"dim"`
	DimIncrement  string                `xml:"dimIncrement"`
	DimIndex      string                `xml:"dimIndex"`
	Registers     []registerLayoutEntry `xml:"register"`
	Clusters      []registerLayoutEntry `xml:"cluster"`
}

// patchSVD applies deterministic corrections to an upstream Puya SVD while
// preserving its formatting and line endings.
func patchSVD(data []byte) ([]byte, error) {
	return normalizeGPIOGroupNames(data)
}

// normalizeGPIOGroupNames gives structurally identical GPIO ports the common
// groupName "GPIO". Puya packs inconsistently omit groupName or use the port
// name, causing generic SVD consumers to generate GPIOA_Type, GPIOB_Type, ...
// for one hardware register layout.
func normalizeGPIOGroupNames(data []byte) ([]byte, error) {
	matches := peripheralElementRE.FindAllIndex(data, -1)
	if len(matches) == 0 {
		return data, nil
	}

	var layouts []*registerLayout
	gpio := make(map[int]bool)
	for i, match := range matches {
		var peripheral patchPeripheral
		if err := xml.Unmarshal(data[match[0]:match[1]], &peripheral); err != nil {
			return nil, fmt.Errorf("decode peripheral: %w", err)
		}
		peripheral.Name = strings.TrimSpace(peripheral.Name)
		if !isGPIOPort(peripheral.Name) {
			continue
		}
		gpio[i] = true
		if peripheral.Registers == nil {
			if strings.TrimSpace(peripheral.DerivedFrom) == "" {
				return nil, fmt.Errorf("GPIO peripheral %s has neither registers nor derivedFrom", peripheral.Name)
			}
			continue
		}
		peripheral.Registers.normalize()
		layouts = append(layouts, peripheral.Registers)
	}
	if len(gpio) == 0 {
		return data, nil
	}
	if len(layouts) == 0 {
		return nil, fmt.Errorf("SVD contains GPIO peripherals but no base GPIO register layout")
	}
	base := layouts[0]
	for _, layout := range layouts[1:] {
		if layout.entryCount() > base.entryCount() {
			base = layout
		}
	}
	for _, layout := range layouts {
		if !base.contains(layout) {
			return nil, fmt.Errorf("GPIO peripherals have conflicting register layouts")
		}
	}

	var out bytes.Buffer
	start := 0
	for i, match := range matches {
		out.Write(data[start:match[0]])
		block := data[match[0]:match[1]]
		if gpio[i] {
			block = setPeripheralGroupName(block, "GPIO")
		}
		out.Write(block)
		start = match[1]
	}
	out.Write(data[start:])
	return out.Bytes(), nil
}

func isGPIOPort(name string) bool {
	return len(name) == 5 && strings.HasPrefix(name, "GPIO") && name[4] >= 'A' && name[4] <= 'Z'
}

func setPeripheralGroupName(block []byte, group string) []byte {
	if groupNameElementRE.Match(block) {
		return groupNameElementRE.ReplaceAll(block, []byte("${1}"+group+"${2}"))
	}
	nameEnd := bytes.Index(block, []byte("</name>"))
	if nameEnd < 0 {
		return block
	}
	nameEnd += len("</name>")
	lineStart := bytes.LastIndexByte(block[:nameEnd], '\n') + 1
	indentEnd := lineStart
	for indentEnd < len(block) && (block[indentEnd] == ' ' || block[indentEnd] == '\t') {
		indentEnd++
	}
	newline := []byte("\n")
	if bytes.Contains(block, []byte("\r\n")) {
		newline = []byte("\r\n")
	}
	insertion := append(append(append([]byte{}, newline...), block[lineStart:indentEnd]...), []byte("<groupName>"+group+"</groupName>")...)
	result := make([]byte, 0, len(block)+len(insertion))
	result = append(result, block[:nameEnd]...)
	result = append(result, insertion...)
	result = append(result, block[nameEnd:]...)
	return result
}

func (layout *registerLayout) normalize() {
	for i := range layout.Registers {
		layout.Registers[i].normalize()
	}
	for i := range layout.Clusters {
		layout.Clusters[i].normalize()
	}
}

func (layout *registerLayout) entryCount() int {
	count := len(layout.Registers) + len(layout.Clusters)
	for i := range layout.Registers {
		count += layout.Registers[i].entryCount()
	}
	for i := range layout.Clusters {
		count += layout.Clusters[i].entryCount()
	}
	return count
}

// contains reports whether other is an identical or partial description of
// layout. Puya occasionally omits a register from one GPIO port (for example
// AFRH on E407 GPIOF); the richest port remains the generated common type.
func (layout *registerLayout) contains(other *registerLayout) bool {
	return entriesContain(layout.Registers, other.Registers) && entriesContain(layout.Clusters, other.Clusters)
}

func entriesContain(all, subset []registerLayoutEntry) bool {
	for i := range subset {
		found := false
		for j := range all {
			if all[j].sameShape(&subset[i]) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (entry *registerLayoutEntry) normalize() {
	entry.Name = strings.TrimSpace(entry.Name)
	entry.Offset = strings.TrimSpace(entry.Offset)
	entry.AddressOffset = strings.TrimSpace(entry.AddressOffset)
	entry.Size = strings.TrimSpace(entry.Size)
	entry.Dim = strings.TrimSpace(entry.Dim)
	entry.DimIncrement = strings.TrimSpace(entry.DimIncrement)
	entry.DimIndex = strings.TrimSpace(entry.DimIndex)
	for i := range entry.Registers {
		entry.Registers[i].normalize()
	}
	for i := range entry.Clusters {
		entry.Clusters[i].normalize()
	}
}

func (entry *registerLayoutEntry) entryCount() int {
	count := len(entry.Registers) + len(entry.Clusters)
	for i := range entry.Registers {
		count += entry.Registers[i].entryCount()
	}
	for i := range entry.Clusters {
		count += entry.Clusters[i].entryCount()
	}
	return count
}

func (entry *registerLayoutEntry) sameShape(other *registerLayoutEntry) bool {
	return entry.Name == other.Name && entry.Offset == other.Offset &&
		entry.AddressOffset == other.AddressOffset && entry.Size == other.Size &&
		entry.Dim == other.Dim && entry.DimIncrement == other.DimIncrement &&
		entry.DimIndex == other.DimIndex &&
		entriesContain(entry.Registers, other.Registers) && entriesContain(other.Registers, entry.Registers) &&
		entriesContain(entry.Clusters, other.Clusters) && entriesContain(other.Clusters, entry.Clusters)
}
