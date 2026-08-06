package updater

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

type svdFieldLocation struct {
	PeripheralName  string
	PeripheralGroup string
	RegisterName    string
	FieldName       string
	BitOffset       uint
	BitWidth        uint
	InsertOffset    int
	ChildIndent     string
	Newline         string
	Pretty          bool
	HasEnumerations bool
}

type svdPeripheralContext struct {
	Name      string
	GroupName string
}

type svdRegisterContext struct {
	Name string
}

type svdFieldContext struct {
	peripheral      *svdPeripheralContext
	register        *svdRegisterContext
	name            string
	bitRange        string
	bitOffset       string
	bitWidth        string
	lsb             string
	msb             string
	contentStart    int
	hasEnumerations bool
	derived         bool
}

type svdScanFrame struct {
	name               string
	text               []byte
	previousPeripheral *svdPeripheralContext
	previousRegister   *svdRegisterContext
	previousField      *svdFieldContext
}

type svdEnumeration struct {
	Name        string
	Description string
	Value       uint64
	conflict    bool
}

type svdInsertion struct {
	offset int
	data   []byte
}

// enrichSVDFromHeaders adds field values from CMSIS device headers to fields
// that do not already contain enumeratedValues. Header values are converted
// from register-positioned C constants to the unshifted values required by the
// CMSIS-SVD format.
func enrichSVDFromHeaders(data []byte, headers []headerSource) ([]byte, int, error) {
	if len(headers) == 0 {
		return data, 0, nil
	}
	fields, err := scanSVDFields(data)
	if err != nil {
		return nil, 0, err
	}
	macros := resolveHeaderMacros(headers)
	if len(fields) == 0 || len(macros) == 0 {
		return data, 0, nil
	}

	fieldsByBase := make(map[string][]*svdFieldLocation)
	for i := range fields {
		field := &fields[i]
		if field.HasEnumerations || field.BitWidth == 0 || field.BitWidth > 64 || field.BitOffset >= 64 || field.BitOffset+field.BitWidth > 64 {
			continue
		}
		for _, base := range fieldHeaderBases(*field) {
			if headerFieldMatches(macros, base, *field) {
				fieldsByBase[base] = append(fieldsByBase[base], field)
			}
		}
	}
	if len(fieldsByBase) == 0 {
		return data, 0, nil
	}

	bases := make([]string, 0, len(fieldsByBase))
	for base := range fieldsByBase {
		bases = append(bases, base)
	}
	sort.Slice(bases, func(i, j int) bool {
		if len(bases[i]) != len(bases[j]) {
			return len(bases[i]) > len(bases[j])
		}
		return bases[i] < bases[j]
	})

	valuesByField := make(map[*svdFieldLocation]map[string]*svdEnumeration)
	macroNames := make([]string, 0, len(macros))
	for name := range macros {
		macroNames = append(macroNames, name)
	}
	sort.Strings(macroNames)
	for _, macroName := range macroNames {
		macro := macros[macroName]
		base, suffix := matchingFieldBase(macroName, bases)
		if base == "" || skipHeaderFieldSuffix(suffix) {
			continue
		}
		for _, field := range fieldsByBase[base] {
			value, ok := localFieldValue(macro.Value, field.BitOffset, field.BitWidth)
			if !ok {
				continue
			}
			values := valuesByField[field]
			if values == nil {
				values = make(map[string]*svdEnumeration)
				valuesByField[field] = values
			}
			key := strings.ToUpper(suffix)
			existing := values[key]
			if existing == nil {
				values[key] = &svdEnumeration{
					Name:        suffix,
					Description: macro.Description,
					Value:       value,
				}
				continue
			}
			if existing.Value != value {
				existing.conflict = true
				continue
			}
			if existing.Description == "" && macro.Description != "" {
				existing.Description = macro.Description
			}
		}
	}

	var insertions []svdInsertion
	valueCount := 0
	for field, values := range valuesByField {
		enumerations := make([]svdEnumeration, 0, len(values))
		for _, value := range values {
			if !value.conflict {
				enumerations = append(enumerations, *value)
			}
		}
		if len(enumerations) == 0 {
			continue
		}
		sort.Slice(enumerations, func(i, j int) bool {
			if enumerations[i].Value != enumerations[j].Value {
				return enumerations[i].Value < enumerations[j].Value
			}
			return enumerations[i].Name < enumerations[j].Name
		})
		insertions = append(insertions, svdInsertion{
			offset: field.InsertOffset,
			data:   renderSVDEnumerations(*field, enumerations),
		})
		valueCount += len(enumerations)
	}
	if len(insertions) == 0 {
		return data, 0, nil
	}

	sort.Slice(insertions, func(i, j int) bool {
		return insertions[i].offset > insertions[j].offset
	})
	for i := 1; i < len(insertions); i++ {
		if insertions[i].offset == insertions[i-1].offset {
			return nil, 0, fmt.Errorf("duplicate SVD insertion offset %d", insertions[i].offset)
		}
	}
	result := append([]byte(nil), data...)
	for _, insertion := range insertions {
		result = append(result, make([]byte, len(insertion.data))...)
		copy(result[insertion.offset+len(insertion.data):], result[insertion.offset:len(result)-len(insertion.data)])
		copy(result[insertion.offset:], insertion.data)
	}
	return result, valueCount, nil
}

func scanSVDFields(data []byte) ([]svdFieldLocation, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var stack []svdScanFrame
	var peripheral *svdPeripheralContext
	var register *svdRegisterContext
	var field *svdFieldContext
	var fields []svdFieldLocation

	for {
		token, err := decoder.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("decode SVD fields: %w", err)
		}
		switch token := token.(type) {
		case xml.StartElement:
			frame := svdScanFrame{name: token.Name.Local}
			switch token.Name.Local {
			case "peripheral":
				frame.previousPeripheral = peripheral
				peripheral = &svdPeripheralContext{}
			case "register":
				frame.previousRegister = register
				register = &svdRegisterContext{}
			case "field":
				frame.previousField = field
				field = &svdFieldContext{
					peripheral:   peripheral,
					register:     register,
					contentStart: int(decoder.InputOffset()),
				}
				for _, attribute := range token.Attr {
					if attribute.Name.Local == "derivedFrom" && strings.TrimSpace(attribute.Value) != "" {
						field.derived = true
					}
				}
			case "enumeratedValues":
				if field != nil {
					field.hasEnumerations = true
				}
			}
			stack = append(stack, frame)

		case xml.CharData:
			if len(stack) != 0 {
				stack[len(stack)-1].text = append(stack[len(stack)-1].text, token...)
			}

		case xml.EndElement:
			if len(stack) == 0 || stack[len(stack)-1].name != token.Name.Local {
				return nil, fmt.Errorf("decode SVD fields: unexpected </%s>", token.Name.Local)
			}
			frame := &stack[len(stack)-1]
			parent := ""
			if len(stack) > 1 {
				parent = stack[len(stack)-2].name
			}
			text := strings.TrimSpace(string(frame.text))
			switch token.Name.Local {
			case "name":
				switch parent {
				case "peripheral":
					peripheral.Name = text
				case "register":
					register.Name = text
				case "field":
					field.name = text
				}
			case "groupName":
				if parent == "peripheral" {
					peripheral.GroupName = text
				}
			case "bitRange":
				if parent == "field" {
					field.bitRange = text
				}
			case "bitOffset":
				if parent == "field" {
					field.bitOffset = text
				}
			case "bitWidth":
				if parent == "field" {
					field.bitWidth = text
				}
			case "lsb":
				if parent == "field" {
					field.lsb = text
				}
			case "msb":
				if parent == "field" {
					field.msb = text
				}
			case "field":
				location, ok := finishSVDField(data, field, int(decoder.InputOffset()))
				if ok {
					fields = append(fields, location)
				}
				field = frame.previousField
			case "register":
				register = frame.previousRegister
			case "peripheral":
				peripheral = frame.previousPeripheral
			}
			stack = stack[:len(stack)-1]
		}
	}
	return fields, nil
}

func finishSVDField(data []byte, field *svdFieldContext, elementEnd int) (svdFieldLocation, bool) {
	if field == nil || field.derived || field.peripheral == nil || field.register == nil {
		return svdFieldLocation{}, false
	}
	bitOffset, bitWidth, ok := svdFieldBits(field)
	if !ok {
		return svdFieldLocation{}, false
	}
	if field.contentStart < 0 || field.contentStart > elementEnd || elementEnd > len(data) {
		return svdFieldLocation{}, false
	}
	relativeClose := bytes.LastIndex(data[field.contentStart:elementEnd], []byte("</"))
	if relativeClose < 0 {
		return svdFieldLocation{}, false
	}
	closeStart := field.contentStart + relativeClose
	insertOffset := closeStart
	for insertOffset > field.contentStart && isXMLWhitespace(data[insertOffset-1]) {
		insertOffset--
	}
	childIndent, newline, pretty := svdFieldFormatting(data, field.contentStart, closeStart)
	return svdFieldLocation{
		PeripheralName:  field.peripheral.Name,
		PeripheralGroup: field.peripheral.GroupName,
		RegisterName:    field.register.Name,
		FieldName:       field.name,
		BitOffset:       bitOffset,
		BitWidth:        bitWidth,
		InsertOffset:    insertOffset,
		ChildIndent:     childIndent,
		Newline:         newline,
		Pretty:          pretty,
		HasEnumerations: field.hasEnumerations,
	}, true
}

func svdFieldBits(field *svdFieldContext) (offset, width uint, ok bool) {
	if field.bitRange != "" {
		bitRange := strings.TrimSpace(field.bitRange)
		if len(bitRange) < 5 || bitRange[0] != '[' || bitRange[len(bitRange)-1] != ']' {
			return 0, 0, false
		}
		parts := strings.Split(bitRange[1:len(bitRange)-1], ":")
		if len(parts) != 2 {
			return 0, 0, false
		}
		msb, msbOK := parseSVDUint(parts[0])
		lsb, lsbOK := parseSVDUint(parts[1])
		if !msbOK || !lsbOK || msb < lsb {
			return 0, 0, false
		}
		return lsb, msb - lsb + 1, true
	}
	if field.bitOffset != "" && field.bitWidth != "" {
		offset, offsetOK := parseSVDUint(field.bitOffset)
		width, widthOK := parseSVDUint(field.bitWidth)
		return offset, width, offsetOK && widthOK && width != 0
	}
	if field.lsb != "" && field.msb != "" {
		lsb, lsbOK := parseSVDUint(field.lsb)
		msb, msbOK := parseSVDUint(field.msb)
		if !lsbOK || !msbOK || msb < lsb {
			return 0, 0, false
		}
		return lsb, msb - lsb + 1, true
	}
	return 0, 0, false
}

func parseSVDUint(value string) (uint, bool) {
	parsed, err := strconv.ParseUint(strings.TrimSpace(value), 0, 32)
	return uint(parsed), err == nil
}

func svdFieldFormatting(data []byte, contentStart, closeStart int) (childIndent, newline string, pretty bool) {
	if contentStart < 0 || contentStart > closeStart || closeStart > len(data) {
		return "", "", false
	}
	newline = "\n"
	if bytes.Contains(data, []byte("\r\n")) {
		newline = "\r\n"
	}
	if !bytes.Contains(data[contentStart:closeStart], []byte("\n")) {
		return "", "", false
	}

	lineStart := bytes.LastIndexByte(data[:closeStart], '\n') + 1
	closingIndent := string(data[lineStart:closeStart])
	if strings.Trim(closingIndent, " \t") != "" {
		closingIndent = ""
	}
	content := data[contentStart:closeStart]
	for position := bytes.IndexByte(content, '\n'); position >= 0 && position+1 < len(content); {
		start := position + 1
		end := start
		for end < len(content) && (content[end] == ' ' || content[end] == '\t' || content[end] == '\r') {
			end++
		}
		if end < len(content) && content[end] == '<' {
			indent := string(content[start:end])
			indent = strings.TrimSuffix(indent, "\r")
			if len(indent) > len(closingIndent) {
				return indent, newline, true
			}
		}
		next := bytes.IndexByte(content[start:], '\n')
		if next < 0 {
			break
		}
		position = start + next
	}
	return closingIndent + "  ", newline, true
}

func isXMLWhitespace(ch byte) bool {
	return ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n'
}

func fieldHeaderBases(field svdFieldLocation) []string {
	if !validHeaderComponent(field.RegisterName) || !validHeaderComponent(field.FieldName) {
		return nil
	}
	peripherals := []string{field.PeripheralGroup, field.PeripheralName}
	seen := make(map[string]bool)
	var bases []string
	for _, peripheral := range peripherals {
		if !validHeaderComponent(peripheral) {
			continue
		}
		base := strings.ToUpper(peripheral + "_" + field.RegisterName + "_" + field.FieldName)
		if !seen[base] {
			seen[base] = true
			bases = append(bases, base)
		}
	}
	return bases
}

func validHeaderComponent(name string) bool {
	if name == "" || !isIdentifierStart(name[0]) {
		return false
	}
	for i := 1; i < len(name); i++ {
		if !isIdentifierPart(name[i]) {
			return false
		}
	}
	return true
}

func matchingFieldBase(macroName string, bases []string) (base, suffix string) {
	for _, candidate := range bases {
		prefix := candidate + "_"
		if strings.HasPrefix(macroName, prefix) {
			return candidate, macroName[len(prefix):]
		}
	}
	return "", ""
}

func skipHeaderFieldSuffix(suffix string) bool {
	if suffix == "" || !validHeaderComponent("X"+suffix) {
		return true
	}
	upper := strings.ToUpper(suffix)
	return upper == "POS" || upper == "MSK" || strings.HasSuffix(upper, "_POS") || strings.HasSuffix(upper, "_MSK")
}

func headerFieldMatches(macros map[string]resolvedHeaderMacro, base string, field svdFieldLocation) bool {
	position, havePosition := macros[base+"_POS"]
	mask, haveMask := macros[base+"_MSK"]
	if !havePosition || !haveMask || position.Value != uint64(field.BitOffset) {
		return false
	}
	expectedMask, ok := svdFieldMask(field.BitOffset, field.BitWidth)
	return ok && mask.Value == expectedMask
}

func svdFieldMask(offset, width uint) (uint64, bool) {
	if width == 0 || width > 64 || offset >= 64 || offset+width > 64 {
		return 0, false
	}
	if width == 64 {
		if offset != 0 {
			return 0, false
		}
		return ^uint64(0), true
	}
	return ((uint64(1) << width) - 1) << offset, true
}

func localFieldValue(value uint64, offset, width uint) (uint64, bool) {
	mask, ok := svdFieldMask(offset, width)
	if !ok {
		return 0, false
	}
	if value & ^mask != 0 {
		return 0, false
	}
	return value >> offset, true
}

func renderSVDEnumerations(field svdFieldLocation, enumerations []svdEnumeration) []byte {
	var out bytes.Buffer
	if !field.Pretty {
		out.WriteString("<enumeratedValues>")
		for _, enumeration := range enumerations {
			out.WriteString("<enumeratedValue><name>")
			writeXMLText(&out, enumeration.Name)
			out.WriteString("</name>")
			if enumeration.Description != "" {
				out.WriteString("<description>")
				writeXMLText(&out, enumeration.Description)
				out.WriteString("</description>")
			}
			out.WriteString("<value>")
			out.WriteString(strconv.FormatUint(enumeration.Value, 10))
			out.WriteString("</value></enumeratedValue>")
		}
		out.WriteString("</enumeratedValues>")
		return out.Bytes()
	}

	valueIndent := field.ChildIndent + "  "
	propertyIndent := valueIndent + "  "
	out.WriteString(field.Newline)
	out.WriteString(field.ChildIndent)
	out.WriteString("<enumeratedValues>")
	for _, enumeration := range enumerations {
		out.WriteString(field.Newline)
		out.WriteString(valueIndent)
		out.WriteString("<enumeratedValue>")
		out.WriteString(field.Newline)
		out.WriteString(propertyIndent)
		out.WriteString("<name>")
		writeXMLText(&out, enumeration.Name)
		out.WriteString("</name>")
		if enumeration.Description != "" {
			out.WriteString(field.Newline)
			out.WriteString(propertyIndent)
			out.WriteString("<description>")
			writeXMLText(&out, enumeration.Description)
			out.WriteString("</description>")
		}
		out.WriteString(field.Newline)
		out.WriteString(propertyIndent)
		out.WriteString("<value>")
		out.WriteString(strconv.FormatUint(enumeration.Value, 10))
		out.WriteString("</value>")
		out.WriteString(field.Newline)
		out.WriteString(valueIndent)
		out.WriteString("</enumeratedValue>")
	}
	out.WriteString(field.Newline)
	out.WriteString(field.ChildIndent)
	out.WriteString("</enumeratedValues>")
	return out.Bytes()
}

func writeXMLText(out *bytes.Buffer, text string) {
	_ = xml.EscapeText(out, []byte(text))
}
