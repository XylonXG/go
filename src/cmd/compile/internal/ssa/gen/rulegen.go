// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build gen

// This program generates Go code that applies rewrite rules to a Value.
// The generated code implements a function of type func (v *Value) bool
// which returns true iff if did something.
// Ideas stolen from Swift: http://www.hpl.hp.com/techreports/Compaq-DEC/WRL-2000-2.html

package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"go/format"
	"io"
	"io/ioutil"
	"log"
	"os"
	"regexp"
	"sort"
	"strings"
)

// rule syntax:
//  sexpr [&& extra conditions] -> [@block] sexpr
//
// sexpr are s-expressions (lisp-like parenthesized groupings)
// sexpr ::= (opcode sexpr*)
//         | variable
//         | <type>
//         | [auxint]
//         | {aux}
//
// aux      ::= variable | {code}
// type     ::= variable | {code}
// variable ::= some token
// opcode   ::= one of the opcodes from ../op.go (without the Op prefix)

// extra conditions is just a chunk of Go that evaluates to a boolean. It may use
// variables declared in the matching sexpr. The variable "v" is predefined to be
// the value matched by the entire rule.

// If multiple rules match, the first one in file order is selected.

var (
	genLog = flag.Bool("log", false, "generate code that logs; for debugging only")
)

type Rule struct {
	rule string
	loc  string // file name & line number
}

func (r Rule) String() string {
	return fmt.Sprintf("rule %q at %s", r.rule, r.loc)
}

// parse returns the matching part of the rule, additional conditions, and the result.
func (r Rule) parse() (match, cond, result string) {
	s := strings.Split(r.rule, "->")
	if len(s) != 2 {
		log.Fatalf("no arrow in %s", r)
	}
	match = strings.TrimSpace(s[0])
	result = strings.TrimSpace(s[1])
	cond = ""
	if i := strings.Index(match, "&&"); i >= 0 {
		cond = strings.TrimSpace(match[i+2:])
		match = strings.TrimSpace(match[:i])
	}
	return match, cond, result
}

func genRules(arch arch) {
	// Open input file.
	text, err := os.Open(arch.name + ".rules")
	if err != nil {
		log.Fatalf("can't read rule file: %v", err)
	}

	// oprules contains a list of rules for each block and opcode
	blockrules := map[string][]Rule{}
	oprules := map[string][]Rule{}

	// read rule file
	scanner := bufio.NewScanner(text)
	rule := ""
	var lineno int
	var ruleLineno int // line number of "->"
	for scanner.Scan() {
		lineno++
		line := scanner.Text()
		if i := strings.Index(line, "//"); i >= 0 {
			// Remove comments. Note that this isn't string safe, so
			// it will truncate lines with // inside strings. Oh well.
			line = line[:i]
		}
		rule += " " + line
		rule = strings.TrimSpace(rule)
		if rule == "" {
			continue
		}
		if !strings.Contains(rule, "->") {
			continue
		}
		if ruleLineno == 0 {
			ruleLineno = lineno
		}
		if strings.HasSuffix(rule, "->") {
			continue
		}
		if unbalanced(rule) {
			continue
		}
		op := strings.Split(rule, " ")[0][1:]
		if op[len(op)-1] == ')' {
			op = op[:len(op)-1] // rule has only opcode, e.g. (ConstNil) -> ...
		}
		loc := fmt.Sprintf("%s.rules:%d", arch.name, ruleLineno)
		if isBlock(op, arch) {
			blockrules[op] = append(blockrules[op], Rule{rule: rule, loc: loc})
		} else {
			oprules[op] = append(oprules[op], Rule{rule: rule, loc: loc})
		}
		rule = ""
		ruleLineno = 0
	}
	if err := scanner.Err(); err != nil {
		log.Fatalf("scanner failed: %v\n", err)
	}
	if unbalanced(rule) {
		log.Fatalf("%s.rules:%d: unbalanced rule: %v\n", arch.name, lineno, rule)
	}

	// Order all the ops.
	var ops []string
	for op := range oprules {
		ops = append(ops, op)
	}
	sort.Strings(ops)

	// Start output buffer, write header.
	w := new(bytes.Buffer)
	fmt.Fprintf(w, "// autogenerated from gen/%s.rules: do not edit!\n", arch.name)
	fmt.Fprintln(w, "// generated with: cd gen; go run *.go")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "package ssa")
	if *genLog {
		fmt.Fprintln(w, "import \"fmt\"")
	}
	fmt.Fprintln(w, "import \"math\"")
	fmt.Fprintln(w, "var _ = math.MinInt8 // in case not otherwise used")

	// Main rewrite routine is a switch on v.Op.
	fmt.Fprintf(w, "func rewriteValue%s(v *Value, config *Config) bool {\n", arch.name)
	fmt.Fprintf(w, "switch v.Op {\n")
	for _, op := range ops {
		fmt.Fprintf(w, "case %s:\n", opName(op, arch))
		fmt.Fprintf(w, "return rewriteValue%s_%s(v, config)\n", arch.name, opName(op, arch))
	}
	fmt.Fprintf(w, "}\n")
	fmt.Fprintf(w, "return false\n")
	fmt.Fprintf(w, "}\n")

	// Generate a routine per op. Note that we don't make one giant routine
	// because it is too big for some compilers.
	for _, op := range ops {
		fmt.Fprintf(w, "func rewriteValue%s_%s(v *Value, config *Config) bool {\n", arch.name, opName(op, arch))
		fmt.Fprintln(w, "b := v.Block")
		fmt.Fprintln(w, "_ = b")
		var canFail bool
		for i, rule := range oprules[op] {
			match, cond, result := rule.parse()
			fmt.Fprintf(w, "// match: %s\n", match)
			fmt.Fprintf(w, "// cond: %s\n", cond)
			fmt.Fprintf(w, "// result: %s\n", result)

			canFail = false
			fmt.Fprintf(w, "for {\n")
			if genMatch(w, arch, match, rule.loc) {
				canFail = true
			}

			if cond != "" {
				fmt.Fprintf(w, "if !(%s) {\nbreak\n}\n", cond)
				canFail = true
			}
			if !canFail && i != len(oprules[op])-1 {
				log.Fatalf("unconditional rule %s is followed by other rules", match)
			}

			genResult(w, arch, result, rule.loc)
			if *genLog {
				fmt.Fprintf(w, "fmt.Println(\"rewrite %s\")\n", rule.loc)
			}
			fmt.Fprintf(w, "return true\n")

			fmt.Fprintf(w, "}\n")
		}
		if canFail {
			fmt.Fprintf(w, "return false\n")
		}
		fmt.Fprintf(w, "}\n")
	}

	// Generate block rewrite function. There are only a few block types
	// so we can make this one function with a switch.
	fmt.Fprintf(w, "func rewriteBlock%s(b *Block) bool {\n", arch.name)
	fmt.Fprintf(w, "switch b.Kind {\n")
	ops = nil
	for op := range blockrules {
		ops = append(ops, op)
	}
	sort.Strings(ops)
	for _, op := range ops {
		fmt.Fprintf(w, "case %s:\n", blockName(op, arch))
		for _, rule := range blockrules[op] {
			match, cond, result := rule.parse()
			fmt.Fprintf(w, "// match: %s\n", match)
			fmt.Fprintf(w, "// cond: %s\n", cond)
			fmt.Fprintf(w, "// result: %s\n", result)

			fmt.Fprintf(w, "for {\n")

			s := split(match[1 : len(match)-1]) // remove parens, then split

			// check match of control value
			if s[1] != "nil" {
				fmt.Fprintf(w, "v := b.Control\n")
				if strings.Contains(s[1], "(") {
					genMatch0(w, arch, s[1], "v", map[string]struct{}{}, false, rule.loc)
				} else {
					fmt.Fprintf(w, "%s := b.Control\n", s[1])
				}
			}

			// assign successor names
			succs := s[2:]
			for i, a := range succs {
				if a != "_" {
					fmt.Fprintf(w, "%s := b.Succs[%d]\n", a, i)
				}
			}

			if cond != "" {
				fmt.Fprintf(w, "if !(%s) {\nbreak\n}\n", cond)
			}

			// Rule matches. Generate result.
			t := split(result[1 : len(result)-1]) // remove parens, then split
			newsuccs := t[2:]

			// Check if newsuccs is the same set as succs.
			m := map[string]bool{}
			for _, succ := range succs {
				if m[succ] {
					log.Fatalf("can't have a repeat successor name %s in %s", succ, rule)
				}
				m[succ] = true
			}
			for _, succ := range newsuccs {
				if !m[succ] {
					log.Fatalf("unknown successor %s in %s", succ, rule)
				}
				delete(m, succ)
			}
			if len(m) != 0 {
				log.Fatalf("unmatched successors %v in %s", m, rule)
			}

			// Modify predecessor lists for no-longer-reachable blocks
			for succ := range m {
				fmt.Fprintf(w, "b.Func.removePredecessor(b, %s)\n", succ)
			}

			fmt.Fprintf(w, "b.Kind = %s\n", blockName(t[0], arch))
			if t[1] == "nil" {
				fmt.Fprintf(w, "b.SetControl(nil)\n")
			} else {
				fmt.Fprintf(w, "b.SetControl(%s)\n", genResult0(w, arch, t[1], new(int), false, false, rule.loc))
			}
			if len(newsuccs) < len(succs) {
				fmt.Fprintf(w, "b.Succs = b.Succs[:%d]\n", len(newsuccs))
			}
			for i, a := range newsuccs {
				fmt.Fprintf(w, "b.Succs[%d] = %s\n", i, a)
			}
			// Update branch prediction
			switch {
			case len(newsuccs) != 2:
				fmt.Fprintln(w, "b.Likely = BranchUnknown")
			case newsuccs[0] == succs[0] && newsuccs[1] == succs[1]:
				// unchanged
			case newsuccs[0] == succs[1] && newsuccs[1] == succs[0]:
				// flipped
				fmt.Fprintln(w, "b.Likely *= -1")
			default:
				// unknown
				fmt.Fprintln(w, "b.Likely = BranchUnknown")
			}

			if *genLog {
				fmt.Fprintf(w, "fmt.Println(\"rewrite %s\")\n", rule.loc)
			}
			fmt.Fprintf(w, "return true\n")

			fmt.Fprintf(w, "}\n")
		}
	}
	fmt.Fprintf(w, "}\n")
	fmt.Fprintf(w, "return false\n")
	fmt.Fprintf(w, "}\n")

	// gofmt result
	b := w.Bytes()
	src, err := format.Source(b)
	if err != nil {
		fmt.Printf("%s\n", b)
		panic(err)
	}

	// Write to file
	err = ioutil.WriteFile("../rewrite"+arch.name+".go", src, 0666)
	if err != nil {
		log.Fatalf("can't write output: %v\n", err)
	}
}

// genMatch returns true if the match can fail.
func genMatch(w io.Writer, arch arch, match string, loc string) bool {
	return genMatch0(w, arch, match, "v", map[string]struct{}{}, true, loc)
}

func genMatch0(w io.Writer, arch arch, match, v string, m map[string]struct{}, top bool, loc string) bool {
	if match[0] != '(' || match[len(match)-1] != ')' {
		panic("non-compound expr in genMatch0: " + match)
	}
	canFail := false

	// split body up into regions. Split by spaces/tabs, except those
	// contained in () or {}.
	s := split(match[1 : len(match)-1]) // remove parens, then split

	// Find op record
	var op opData
	for _, x := range genericOps {
		if x.name == s[0] {
			op = x
			break
		}
	}
	for _, x := range arch.ops {
		if x.name == s[0] {
			op = x
			break
		}
	}
	if op.name == "" {
		log.Fatalf("%s: unknown op %s", loc, s[0])
	}

	// check op
	if !top {
		fmt.Fprintf(w, "if %s.Op != %s {\nbreak\n}\n", v, opName(s[0], arch))
		canFail = true
	}

	// check type/aux/args
	argnum := 0
	for _, a := range s[1:] {
		if a[0] == '<' {
			// type restriction
			t := a[1 : len(a)-1] // remove <>
			if !isVariable(t) {
				// code. We must match the results of this code.
				fmt.Fprintf(w, "if %s.Type != %s {\nbreak\n}\n", v, t)
				canFail = true
			} else {
				// variable
				if _, ok := m[t]; ok {
					// must match previous variable
					fmt.Fprintf(w, "if %s.Type != %s {\nbreak\n}\n", v, t)
					canFail = true
				} else {
					m[t] = struct{}{}
					fmt.Fprintf(w, "%s := %s.Type\n", t, v)
				}
			}
		} else if a[0] == '[' {
			// auxint restriction
			switch op.aux {
			case "Bool", "Int8", "Int16", "Int32", "Int64", "Int128", "Float32", "Float64", "SymOff", "SymValAndOff", "SymInt32":
			default:
				log.Fatalf("%s: op %s %s can't have auxint", loc, op.name, op.aux)
			}
			x := a[1 : len(a)-1] // remove []
			if !isVariable(x) {
				// code
				fmt.Fprintf(w, "if %s.AuxInt != %s {\nbreak\n}\n", v, x)
				canFail = true
			} else {
				// variable
				if _, ok := m[x]; ok {
					fmt.Fprintf(w, "if %s.AuxInt != %s {\nbreak\n}\n", v, x)
					canFail = true
				} else {
					m[x] = struct{}{}
					fmt.Fprintf(w, "%s := %s.AuxInt\n", x, v)
				}
			}
		} else if a[0] == '{' {
			// aux restriction
			switch op.aux {
			case "String", "Sym", "SymOff", "SymValAndOff", "SymInt32":
			default:
				log.Fatalf("%s: op %s %s can't have aux", loc, op.name, op.aux)
			}
			x := a[1 : len(a)-1] // remove {}
			if !isVariable(x) {
				// code
				fmt.Fprintf(w, "if %s.Aux != %s {\nbreak\n}\n", v, x)
				canFail = true
			} else {
				// variable
				if _, ok := m[x]; ok {
					fmt.Fprintf(w, "if %s.Aux != %s {\nbreak\n}\n", v, x)
					canFail = true
				} else {
					m[x] = struct{}{}
					fmt.Fprintf(w, "%s := %s.Aux\n", x, v)
				}
			}
		} else if a == "_" {
			argnum++
		} else if !strings.Contains(a, "(") {
			// leaf variable
			if _, ok := m[a]; ok {
				// variable already has a definition. Check whether
				// the old definition and the new definition match.
				// For example, (add x x).  Equality is just pointer equality
				// on Values (so cse is important to do before lowering).
				fmt.Fprintf(w, "if %s != %s.Args[%d] {\nbreak\n}\n", a, v, argnum)
				canFail = true
			} else {
				// remember that this variable references the given value
				m[a] = struct{}{}
				fmt.Fprintf(w, "%s := %s.Args[%d]\n", a, v, argnum)
			}
			argnum++
		} else {
			// compound sexpr
			var argname string
			colon := strings.Index(a, ":")
			openparen := strings.Index(a, "(")
			if colon >= 0 && openparen >= 0 && colon < openparen {
				// rule-specified name
				argname = a[:colon]
				a = a[colon+1:]
			} else {
				// autogenerated name
				argname = fmt.Sprintf("%s_%d", v, argnum)
			}
			fmt.Fprintf(w, "%s := %s.Args[%d]\n", argname, v, argnum)
			if genMatch0(w, arch, a, argname, m, false, loc) {
				canFail = true
			}
			argnum++
		}
	}
	if op.argLength == -1 {
		fmt.Fprintf(w, "if len(%s.Args) != %d {\nbreak\n}\n", v, argnum)
		canFail = true
	} else if int(op.argLength) != argnum {
		log.Fatalf("%s: op %s should have %d args, has %d", loc, op.name, op.argLength, argnum)
	}
	return canFail
}

func genResult(w io.Writer, arch arch, result string, loc string) {
	move := false
	if result[0] == '@' {
		// parse @block directive
		s := strings.SplitN(result[1:], " ", 2)
		fmt.Fprintf(w, "b = %s\n", s[0])
		result = s[1]
		move = true
	}
	genResult0(w, arch, result, new(int), true, move, loc)
}
func genResult0(w io.Writer, arch arch, result string, alloc *int, top, move bool, loc string) string {
	// TODO: when generating a constant result, use f.constVal to avoid
	// introducing copies just to clean them up again.
	if result[0] != '(' {
		// variable
		if top {
			// It in not safe in general to move a variable between blocks
			// (and particularly not a phi node).
			// Introduce a copy.
			fmt.Fprintf(w, "v.reset(OpCopy)\n")
			fmt.Fprintf(w, "v.Type = %s.Type\n", result)
			fmt.Fprintf(w, "v.AddArg(%s)\n", result)
		}
		return result
	}

	s := split(result[1 : len(result)-1]) // remove parens, then split

	// Find op record
	var op opData
	for _, x := range genericOps {
		if x.name == s[0] {
			op = x
			break
		}
	}
	for _, x := range arch.ops {
		if x.name == s[0] {
			op = x
			break
		}
	}
	if op.name == "" {
		log.Fatalf("%s: unknown op %s", loc, s[0])
	}

	// Find the type of the variable.
	var opType string
	var typeOverride bool
	for _, a := range s[1:] {
		if a[0] == '<' {
			// type restriction
			opType = a[1 : len(a)-1] // remove <>
			typeOverride = true
			break
		}
	}
	if opType == "" {
		// find default type, if any
		for _, op := range arch.ops {
			if op.name == s[0] && op.typ != "" {
				opType = typeName(op.typ)
				break
			}
		}
	}
	if opType == "" {
		for _, op := range genericOps {
			if op.name == s[0] && op.typ != "" {
				opType = typeName(op.typ)
				break
			}
		}
	}
	var v string
	if top && !move {
		v = "v"
		fmt.Fprintf(w, "v.reset(%s)\n", opName(s[0], arch))
		if typeOverride {
			fmt.Fprintf(w, "v.Type = %s\n", opType)
		}
	} else {
		if opType == "" {
			log.Fatalf("sub-expression %s (op=%s) must have a type", result, s[0])
		}
		v = fmt.Sprintf("v%d", *alloc)
		*alloc++
		fmt.Fprintf(w, "%s := b.NewValue0(v.Line, %s, %s)\n", v, opName(s[0], arch), opType)
		if move && top {
			// Rewrite original into a copy
			fmt.Fprintf(w, "v.reset(OpCopy)\n")
			fmt.Fprintf(w, "v.AddArg(%s)\n", v)
		}
	}
	argnum := 0
	for _, a := range s[1:] {
		if a[0] == '<' {
			// type restriction, handled above
		} else if a[0] == '[' {
			// auxint restriction
			switch op.aux {
			case "Bool", "Int8", "Int16", "Int32", "Int64", "Int128", "Float32", "Float64", "SymOff", "SymValAndOff", "SymInt32":
			default:
				log.Fatalf("%s: op %s %s can't have auxint", loc, op.name, op.aux)
			}
			x := a[1 : len(a)-1] // remove []
			fmt.Fprintf(w, "%s.AuxInt = %s\n", v, x)
		} else if a[0] == '{' {
			// aux restriction
			switch op.aux {
			case "String", "Sym", "SymOff", "SymValAndOff", "SymInt32":
			default:
				log.Fatalf("%s: op %s %s can't have aux", loc, op.name, op.aux)
			}
			x := a[1 : len(a)-1] // remove {}
			fmt.Fprintf(w, "%s.Aux = %s\n", v, x)
		} else {
			// regular argument (sexpr or variable)
			x := genResult0(w, arch, a, alloc, false, move, loc)
			fmt.Fprintf(w, "%s.AddArg(%s)\n", v, x)
			argnum++
		}
	}
	if op.argLength != -1 && int(op.argLength) != argnum {
		log.Fatalf("%s: op %s should have %d args, has %d", loc, op.name, op.argLength, argnum)
	}

	return v
}

func split(s string) []string {
	var r []string

outer:
	for s != "" {
		d := 0               // depth of ({[<
		var open, close byte // opening and closing markers ({[< or )}]>
		nonsp := false       // found a non-space char so far
		for i := 0; i < len(s); i++ {
			switch {
			case d == 0 && s[i] == '(':
				open, close = '(', ')'
				d++
			case d == 0 && s[i] == '<':
				open, close = '<', '>'
				d++
			case d == 0 && s[i] == '[':
				open, close = '[', ']'
				d++
			case d == 0 && s[i] == '{':
				open, close = '{', '}'
				d++
			case d == 0 && (s[i] == ' ' || s[i] == '\t'):
				if nonsp {
					r = append(r, strings.TrimSpace(s[:i]))
					s = s[i:]
					continue outer
				}
			case d > 0 && s[i] == open:
				d++
			case d > 0 && s[i] == close:
				d--
			default:
				nonsp = true
			}
		}
		if d != 0 {
			panic("imbalanced expression: " + s)
		}
		if nonsp {
			r = append(r, strings.TrimSpace(s))
		}
		break
	}
	return r
}

// isBlock returns true if this op is a block opcode.
func isBlock(name string, arch arch) bool {
	for _, b := range genericBlocks {
		if b.name == name {
			return true
		}
	}
	for _, b := range arch.blocks {
		if b.name == name {
			return true
		}
	}
	return false
}

// opName converts from an op name specified in a rule file to an Op enum.
// if the name matches a generic op, returns "Op" plus the specified name.
// Otherwise, returns "Op" plus arch name plus op name.
func opName(name string, arch arch) string {
	for _, op := range genericOps {
		if op.name == name {
			return "Op" + name
		}
	}
	return "Op" + arch.name + name
}

func blockName(name string, arch arch) string {
	for _, b := range genericBlocks {
		if b.name == name {
			return "Block" + name
		}
	}
	return "Block" + arch.name + name
}

// typeName returns the string to use to generate a type.
func typeName(typ string) string {
	switch typ {
	case "Flags", "Mem", "Void", "Int128":
		return "Type" + typ
	default:
		return "config.fe.Type" + typ + "()"
	}
}

// unbalanced returns true if there aren't the same number of ( and ) in the string.
func unbalanced(s string) bool {
	var left, right int
	for _, c := range s {
		if c == '(' {
			left++
		}
		if c == ')' {
			right++
		}
	}
	return left != right
}

// isVariable reports whether s is a single Go alphanumeric identifier.
func isVariable(s string) bool {
	b, err := regexp.MatchString("^[A-Za-z_][A-Za-z_0-9]*$", s)
	if err != nil {
		panic("bad variable regexp")
	}
	return b
}
