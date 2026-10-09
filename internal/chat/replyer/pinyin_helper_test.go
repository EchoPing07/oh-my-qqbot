package replyer

import "github.com/mozillazg/go-pinyin"

// singlePinyin returns the Tone3 pinyin of one rune.
func singlePinyin(r rune) []string {
	return pinyin.SinglePinyin(r, pinyin.Args{Style: pinyin.Tone3})
}
