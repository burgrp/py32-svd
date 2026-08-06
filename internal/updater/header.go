package updater

import (
	"math"
	"sort"
	"strconv"
	"strings"
)

const (
	maxHeaderExpressionLength = 4096
	maxHeaderExpressionTokens = 256
	maxHeaderMacroDepth       = 128
)

type headerSource struct {
	Path string
	Data []byte
}

type headerDefinition struct {
	Expression  string
	Description string
}

type resolvedHeaderMacro struct {
	Value       uint64
	Description string
}

// resolveHeaderMacros parses and evaluates object-like macros from all headers.
// A macro is returned only when every header that can evaluate it agrees on its
// value. Definitions that depend on unsupported C syntax are ignored.
func resolveHeaderMacros(headers []headerSource) map[string]resolvedHeaderMacro {
	type consensus struct {
		macro               resolvedHeaderMacro
		seen                int
		conflict            bool
		descriptionConflict bool
	}

	merged := make(map[string]*consensus)
	for _, header := range headers {
		definitions := parseHeaderDefinitions(header.Data)
		evaluator := headerMacroEvaluator{
			definitions: definitions,
			memo:        make(map[string]headerMacroResult),
			active:      make(map[string]bool),
		}

		names := make([]string, 0, len(definitions))
		for name := range definitions {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			value, description, ok := evaluator.evaluate(name)
			if !ok {
				continue
			}
			key := strings.ToUpper(name)
			entry := merged[key]
			if entry == nil {
				merged[key] = &consensus{macro: resolvedHeaderMacro{
					Value:       value,
					Description: description,
				}, seen: 1}
				continue
			}
			entry.seen++
			if entry.macro.Value != value {
				entry.conflict = true
				continue
			}
			if entry.macro.Description != "" && description != "" && entry.macro.Description != description {
				entry.macro.Description = ""
				entry.descriptionConflict = true
			} else if !entry.descriptionConflict && entry.macro.Description == "" && description != "" {
				entry.macro.Description = description
			}
		}
	}

	result := make(map[string]resolvedHeaderMacro)
	for name, entry := range merged {
		if !entry.conflict && entry.seen == len(headers) {
			result[name] = entry.macro
		}
	}
	return result
}

func parseHeaderDefinitions(data []byte) map[string][]headerDefinition {
	lines := headerLogicalLines(data)
	definitions := make(map[string][]headerDefinition)
	for i, line := range lines {
		name, expression, description, ok := parseHeaderDefine(line)
		if !ok {
			continue
		}
		if description == "" && i+1 < len(lines) {
			description = trailingHeaderComment(lines, i+1)
		}
		definition := headerDefinition{
			Expression:  expression,
			Description: description,
		}
		duplicate := false
		for _, existing := range definitions[name] {
			if existing.Expression == definition.Expression && existing.Description == definition.Description {
				duplicate = true
				break
			}
		}
		if !duplicate {
			definitions[name] = append(definitions[name], definition)
		}
	}
	return definitions
}

func headerLogicalLines(data []byte) []string {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	physical := strings.Split(text, "\n")
	lines := make([]string, 0, len(physical))
	var current strings.Builder
	for _, line := range physical {
		trimmed := strings.TrimRight(line, " \t")
		continued := strings.HasSuffix(trimmed, "\\")
		if continued {
			trimmed = strings.TrimSuffix(trimmed, "\\")
		}
		if current.Len() != 0 {
			current.WriteByte(' ')
		}
		current.WriteString(trimmed)
		if continued {
			continue
		}
		lines = append(lines, current.String())
		current.Reset()
	}
	if current.Len() != 0 {
		lines = append(lines, current.String())
	}
	return lines
}

func parseHeaderDefine(line string) (name, expression, description string, ok bool) {
	line = strings.TrimLeft(line, " \t")
	if !strings.HasPrefix(line, "#") {
		return "", "", "", false
	}
	line = strings.TrimLeft(line[1:], " \t")
	if !strings.HasPrefix(line, "define") || (len(line) > len("define") && !isHeaderSpace(line[len("define")])) {
		return "", "", "", false
	}
	line = strings.TrimLeft(line[len("define"):], " \t")
	if line == "" || !isIdentifierStart(line[0]) {
		return "", "", "", false
	}
	end := 1
	for end < len(line) && isIdentifierPart(line[end]) {
		end++
	}
	name = line[:end]
	// A left parenthesis immediately after the name denotes a function-like
	// macro. Parenthesized object-like values always have separating space.
	if end < len(line) && line[end] == '(' {
		return "", "", "", false
	}
	remainder := strings.TrimSpace(line[end:])
	if remainder == "" {
		return "", "", "", false
	}
	expression, description = splitHeaderExpression(remainder)
	expression = strings.TrimSpace(expression)
	if expression == "" || len(expression) > maxHeaderExpressionLength {
		return "", "", "", false
	}
	return name, expression, description, true
}

func splitHeaderExpression(text string) (expression, description string) {
	comment := len(text)
	if index := strings.Index(text, "/*"); index >= 0 && index < comment {
		comment = index
	}
	if index := strings.Index(text, "//"); index >= 0 && index < comment {
		comment = index
	}
	if comment == len(text) {
		return text, ""
	}
	return text[:comment], cleanHeaderComment(text[comment:])
}

func trailingHeaderComment(lines []string, start int) string {
	line := strings.TrimSpace(lines[start])
	if !(strings.HasPrefix(line, "/*!<") || strings.HasPrefix(line, "/**<")) {
		return ""
	}
	var comment strings.Builder
	for i := start; i < len(lines); i++ {
		if comment.Len() != 0 {
			comment.WriteByte(' ')
		}
		comment.WriteString(strings.TrimSpace(lines[i]))
		if strings.Contains(lines[i], "*/") {
			break
		}
	}
	return cleanHeaderComment(comment.String())
}

func cleanHeaderComment(comment string) string {
	comment = strings.TrimSpace(comment)
	comment = strings.TrimPrefix(comment, "//")
	comment = strings.TrimPrefix(comment, "/*")
	comment = strings.TrimSuffix(comment, "*/")
	comment = strings.TrimSpace(comment)
	comment = strings.TrimPrefix(comment, "!")
	comment = strings.TrimPrefix(comment, "*")
	comment = strings.TrimPrefix(comment, "<")
	comment = strings.TrimSpace(comment)
	comment = strings.Join(strings.Fields(comment), " ")
	if isHeaderNumericComment(comment) {
		return ""
	}
	return comment
}

func isHeaderNumericComment(comment string) bool {
	if comment == "" {
		return false
	}
	_, ok := parseHeaderInteger(comment)
	return ok
}

func isHeaderSpace(ch byte) bool {
	return ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n'
}

func isIdentifierStart(ch byte) bool {
	return ch == '_' || ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z'
}

func isIdentifierPart(ch byte) bool {
	return isIdentifierStart(ch) || ch >= '0' && ch <= '9'
}

type headerMacroResult struct {
	value       uint64
	description string
	ok          bool
	complete    bool
}

type headerMacroEvaluator struct {
	definitions map[string][]headerDefinition
	memo        map[string]headerMacroResult
	active      map[string]bool
}

func (e *headerMacroEvaluator) evaluate(name string) (uint64, string, bool) {
	if result, ok := e.memo[name]; ok && result.complete {
		return result.value, result.description, result.ok
	}
	if e.active[name] || len(e.active) >= maxHeaderMacroDepth {
		return 0, "", false
	}
	definitions := e.definitions[name]
	if len(definitions) == 0 {
		return 0, "", false
	}

	e.active[name] = true
	defer delete(e.active, name)

	var value uint64
	var description string
	descriptionConflict := false
	haveValue := false
	for _, definition := range definitions {
		candidate, ok := evaluateHeaderExpression(definition.Expression, func(identifier string) (uint64, bool) {
			resolved, _, ok := e.evaluate(identifier)
			return resolved, ok
		})
		if !ok {
			result := headerMacroResult{complete: true}
			e.memo[name] = result
			return 0, "", false
		}
		if haveValue && candidate != value {
			result := headerMacroResult{complete: true}
			e.memo[name] = result
			return 0, "", false
		}
		value = candidate
		haveValue = true
		if description != "" && definition.Description != "" && description != definition.Description {
			description = ""
			descriptionConflict = true
		} else if !descriptionConflict && description == "" && definition.Description != "" {
			description = definition.Description
		}
	}
	result := headerMacroResult{
		value:       value,
		description: description,
		ok:          haveValue,
		complete:    true,
	}
	e.memo[name] = result
	return result.value, result.description, result.ok
}

type headerTokenKind uint8

const (
	headerTokenEOF headerTokenKind = iota
	headerTokenInteger
	headerTokenIdentifier
	headerTokenOperator
	headerTokenLeftParen
	headerTokenRightParen
)

type headerToken struct {
	kind  headerTokenKind
	text  string
	value uint64
}

func evaluateHeaderExpression(expression string, resolve func(string) (uint64, bool)) (uint64, bool) {
	tokens, ok := tokenizeHeaderExpression(expression)
	if !ok {
		return 0, false
	}
	parser := headerExpressionParser{tokens: tokens, resolve: resolve}
	value, ok := parser.parseExpression(1)
	if !ok || parser.peek().kind != headerTokenEOF {
		return 0, false
	}
	return value, true
}

func tokenizeHeaderExpression(expression string) ([]headerToken, bool) {
	tokens := make([]headerToken, 0, 16)
	for i := 0; i < len(expression); {
		if isHeaderSpace(expression[i]) {
			i++
			continue
		}
		if len(tokens) >= maxHeaderExpressionTokens {
			return nil, false
		}
		ch := expression[i]
		if ch >= '0' && ch <= '9' {
			start := i
			i++
			for i < len(expression) && (isIdentifierPart(expression[i])) {
				i++
			}
			literal := expression[start:i]
			value, ok := parseHeaderInteger(literal)
			if !ok {
				return nil, false
			}
			tokens = append(tokens, headerToken{kind: headerTokenInteger, text: literal, value: value})
			continue
		}
		if isIdentifierStart(ch) {
			start := i
			i++
			for i < len(expression) && isIdentifierPart(expression[i]) {
				i++
			}
			tokens = append(tokens, headerToken{kind: headerTokenIdentifier, text: expression[start:i]})
			continue
		}
		switch ch {
		case '(':
			tokens = append(tokens, headerToken{kind: headerTokenLeftParen, text: "("})
			i++
		case ')':
			tokens = append(tokens, headerToken{kind: headerTokenRightParen, text: ")"})
			i++
		case '<', '>':
			if i+1 >= len(expression) || expression[i+1] != ch {
				return nil, false
			}
			tokens = append(tokens, headerToken{kind: headerTokenOperator, text: expression[i : i+2]})
			i += 2
		case '|', '^', '&', '+', '-', '*', '/', '%', '~':
			tokens = append(tokens, headerToken{kind: headerTokenOperator, text: expression[i : i+1]})
			i++
		default:
			return nil, false
		}
	}
	tokens = append(tokens, headerToken{kind: headerTokenEOF})
	return tokens, true
}

func parseHeaderInteger(literal string) (uint64, bool) {
	literal = strings.TrimSpace(literal)
	end := len(literal)
	for end > 0 {
		switch literal[end-1] {
		case 'u', 'U', 'l', 'L':
			end--
		default:
			goto suffixRemoved
		}
	}

suffixRemoved:
	if end == 0 {
		return 0, false
	}
	value, err := strconv.ParseUint(literal[:end], 0, 64)
	return value, err == nil
}

type headerExpressionParser struct {
	tokens  []headerToken
	pos     int
	resolve func(string) (uint64, bool)
}

func (p *headerExpressionParser) peek() headerToken {
	if p.pos >= len(p.tokens) {
		return headerToken{kind: headerTokenEOF}
	}
	return p.tokens[p.pos]
}

func (p *headerExpressionParser) take() headerToken {
	token := p.peek()
	if p.pos < len(p.tokens) {
		p.pos++
	}
	return token
}

func (p *headerExpressionParser) parseExpression(minPrecedence int) (uint64, bool) {
	left, ok := p.parseUnary()
	if !ok {
		return 0, false
	}
	for {
		operator := p.peek()
		precedence := headerOperatorPrecedence(operator)
		if precedence < minPrecedence {
			break
		}
		p.take()
		right, ok := p.parseExpression(precedence + 1)
		if !ok {
			return 0, false
		}
		left, ok = applyHeaderOperator(operator.text, left, right)
		if !ok {
			return 0, false
		}
	}
	return left, true
}

func (p *headerExpressionParser) parseUnary() (uint64, bool) {
	token := p.peek()
	if token.kind == headerTokenOperator && (token.text == "+" || token.text == "-" || token.text == "~") {
		p.take()
		value, ok := p.parseUnary()
		if !ok {
			return 0, false
		}
		switch token.text {
		case "+":
			return value, true
		case "-":
			if value != 0 {
				return 0, false
			}
			return 0, true
		case "~":
			return ^value, true
		}
	}

	if width, end, ok := p.castAtCurrentPosition(); ok {
		p.pos = end + 1
		value, ok := p.parseUnary()
		if !ok {
			return 0, false
		}
		if width != 0 && width < 64 {
			value &= uint64(1)<<width - 1
		}
		return value, true
	}
	return p.parsePrimary()
}

func (p *headerExpressionParser) parsePrimary() (uint64, bool) {
	token := p.take()
	switch token.kind {
	case headerTokenInteger:
		return token.value, true
	case headerTokenIdentifier:
		if isHeaderConstantWrapper(token.text) && p.peek().kind == headerTokenLeftParen {
			p.take()
			value, ok := p.parseExpression(1)
			if !ok || p.take().kind != headerTokenRightParen {
				return 0, false
			}
			return value, true
		}
		return p.resolve(token.text)
	case headerTokenLeftParen:
		value, ok := p.parseExpression(1)
		if !ok || p.take().kind != headerTokenRightParen {
			return 0, false
		}
		return value, true
	default:
		return 0, false
	}
}

func (p *headerExpressionParser) castAtCurrentPosition() (width uint, end int, ok bool) {
	if p.peek().kind != headerTokenLeftParen {
		return 0, 0, false
	}
	var words []string
	for i := p.pos + 1; i < len(p.tokens); i++ {
		token := p.tokens[i]
		if token.kind == headerTokenRightParen {
			if len(words) == 0 {
				return 0, 0, false
			}
			width, ok := headerCastWidth(words)
			return width, i, ok
		}
		if token.kind == headerTokenOperator && token.text == "*" {
			words = append(words, "*")
			continue
		}
		if token.kind != headerTokenIdentifier {
			return 0, 0, false
		}
		words = append(words, token.text)
	}
	return 0, 0, false
}

func headerCastWidth(words []string) (uint, bool) {
	typeLike := false
	width := uint(0)
	for _, word := range words {
		lower := strings.ToLower(word)
		switch lower {
		case "const", "volatile", "signed", "unsigned", "char", "short", "int", "long", "*", "__i", "__o", "__io":
			typeLike = true
		case "uint8_t", "int8_t":
			typeLike = true
			width = 8
		case "uint16_t", "int16_t":
			typeLike = true
			width = 16
		case "uint32_t", "int32_t":
			typeLike = true
			width = 32
		case "uint64_t", "int64_t":
			typeLike = true
			width = 64
		default:
			if !strings.HasSuffix(lower, "_t") {
				return 0, false
			}
			typeLike = true
		}
	}
	return width, typeLike
}

func isHeaderConstantWrapper(name string) bool {
	switch name {
	case "INT8_C", "UINT8_C", "INT16_C", "UINT16_C", "INT32_C", "UINT32_C", "INT64_C", "UINT64_C":
		return true
	default:
		return false
	}
}

func headerOperatorPrecedence(token headerToken) int {
	if token.kind != headerTokenOperator {
		return 0
	}
	switch token.text {
	case "|":
		return 1
	case "^":
		return 2
	case "&":
		return 3
	case "<<", ">>":
		return 4
	case "+", "-":
		return 5
	case "*", "/", "%":
		return 6
	default:
		return 0
	}
}

func applyHeaderOperator(operator string, left, right uint64) (uint64, bool) {
	switch operator {
	case "|":
		return left | right, true
	case "^":
		return left ^ right, true
	case "&":
		return left & right, true
	case "<<":
		if right >= 64 || left > math.MaxUint64>>right {
			return 0, false
		}
		return left << right, true
	case ">>":
		if right >= 64 {
			return 0, false
		}
		return left >> right, true
	case "+":
		if left > math.MaxUint64-right {
			return 0, false
		}
		return left + right, true
	case "-":
		if left < right {
			return 0, false
		}
		return left - right, true
	case "*":
		if right != 0 && left > math.MaxUint64/right {
			return 0, false
		}
		return left * right, true
	case "/":
		if right == 0 {
			return 0, false
		}
		return left / right, true
	case "%":
		if right == 0 {
			return 0, false
		}
		return left % right, true
	default:
		return 0, false
	}
}
