package earthfile2llb

import (
	"fmt"
	"strings"

	"github.com/antlr/antlr4/runtime/Go/antlr"
	"github.com/earthly/earthly/earthfile2llb/parser"
)

// lexer is a lexer for an earthly file, which also emits indentation
// and dedentation tokens.
type lexer struct {
	*parser.EarthLexer
	prevIndentLevel                              int
	indentLevel                                  int
	afterNewLine                                 bool
	tokenQueue                                   []antlr.Token
	wsChannel, wsStart, wsStop, wsLine, wsColumn int
}

func newLexer(input antlr.CharStream) antlr.Lexer {
	fmt.Printf("-- calling newLexer\n")
	l := new(lexer)
	l.EarthLexer = parser.NewEarthLexer(input)
	return l
}

func (l *lexer) NextToken() antlr.Token {
	peek := l.EarthLexer.NextToken()
	i := l.EarthLexer.GetInputStream().Index()
	is := l.EarthLexer.GetInputStream()
	fmt.Printf("calling NextToken() got type=%v data=%v index=%d ptr=%p\n", peek.GetTokenType(), peek, i, is)

	ret := peek
	tokenType := peek.GetTokenType()
	switch tokenType {
	case parser.EarthLexerWS:
		if l.afterNewLine {
			l.indentLevel++
		}
		l.wsChannel, l.wsStart, l.wsStop, l.wsLine, l.wsColumn =
			peek.GetChannel(), peek.GetStart(), peek.GetStop(), peek.GetLine(), peek.GetColumn()
	case parser.EarthLexerNL:
		l.indentLevel = 0
		l.afterNewLine = true
	case parser.EarthLexerHereDoc:
		panic("TODO")
	default:
		if tokenType == parser.EarthLexerAtom {
			s := peek.GetText()
			fmt.Printf("here with %q\n", s)
			if strings.HasPrefix(s, "<<") {
				heredoc := "EOF" // TODO parse this

				start := peek.GetStart()
				start += len("<<" + heredoc + "\n")
				n := 19                // TODO figure this number out programatically
				end := start + (n - 1) // end is inclusive, change to exclusive

				is := l.GetInputStream()
				fmt.Printf("index is %d\n", is.Index())

				s := is.GetText(start, end)
				fmt.Printf("got %q\n", s)

				n = strings.Index(s, "EOF")
				if n < 0 {
					panic("EOF not found")
				}
				s = s[:n]
				n += len("EOF")
				fmt.Printf("fast forward %d chars\n", n)

				l.TokenStartCharIndex = start + n
				// TODO also need to set the line and column here (otherwise parsing error message will point to wrong location)

				fmt.Printf("set token to %q\n", s)
				ret.SetText(s)
				l.GetInputStream().Seek(start + n)

				l.PopMode() // Pop COMMAND

				return ret
			}
		}

		if l.afterNewLine {
			if l.prevIndentLevel < l.indentLevel {
				l.tokenQueue = append(l.tokenQueue, l.GetTokenFactory().Create(
					l.GetTokenSourceCharStreamPair(), parser.EarthLexerINDENT, "",
					l.wsChannel, l.wsStart, l.wsStop, l.wsLine, l.wsColumn))
			} else if l.prevIndentLevel > l.indentLevel {
				l.tokenQueue = append(l.tokenQueue, l.GetTokenFactory().Create(
					l.GetTokenSourceCharStreamPair(), parser.EarthLexerDEDENT, "",
					l.wsChannel, l.wsStart, l.wsStop, l.wsLine, l.wsColumn))
				l.PopMode() // Pop RECIPE mode.
			}
		}
		l.prevIndentLevel = l.indentLevel
		l.afterNewLine = false
	}
	if len(l.tokenQueue) > 0 {
		l.tokenQueue = append(l.tokenQueue, peek)
		ret = l.tokenQueue[0]
		l.tokenQueue = l.tokenQueue[1:]
	}
	return ret
}
