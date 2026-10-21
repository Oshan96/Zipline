package main

import "strings"

// Language codes. Streams name their tracks' languages in BCP 47 ("en", "pt-BR", "es-419"); MP4
// wants ISO 639-2/T three-letter codes in each track's header ("deu") and Matroska the older
// ISO 639-2/B ones ("ger"). Only the 20 languages below differ between the two.

// iso639_1 maps two-letter codes to ISO 639-2/T.
var iso639_1 = map[string]string{
	"aa": "aar", "ab": "abk", "ae": "ave", "af": "afr", "ak": "aka", "am": "amh", "an": "arg", "ar": "ara", "as": "asm", "av": "ava",
	"ay": "aym", "az": "aze", "ba": "bak", "be": "bel", "bg": "bul", "bh": "bih", "bi": "bis", "bm": "bam", "bn": "ben", "bo": "bod",
	"br": "bre", "bs": "bos", "ca": "cat", "ce": "che", "ch": "cha", "co": "cos", "cr": "cre", "cs": "ces", "cu": "chu", "cv": "chv",
	"cy": "cym", "da": "dan", "de": "deu", "dv": "div", "dz": "dzo", "ee": "ewe", "el": "ell", "en": "eng", "eo": "epo", "es": "spa",
	"et": "est", "eu": "eus", "fa": "fas", "ff": "ful", "fi": "fin", "fj": "fij", "fo": "fao", "fr": "fra", "fy": "fry", "ga": "gle",
	"gd": "gla", "gl": "glg", "gn": "grn", "gu": "guj", "gv": "glv", "ha": "hau", "he": "heb", "hi": "hin", "ho": "hmo", "hr": "hrv",
	"ht": "hat", "hu": "hun", "hy": "hye", "hz": "her", "ia": "ina", "id": "ind", "ie": "ile", "ig": "ibo", "ii": "iii", "ik": "ipk",
	"io": "ido", "is": "isl", "it": "ita", "iu": "iku", "ja": "jpn", "jv": "jav", "ka": "kat", "kg": "kon", "ki": "kik", "kj": "kua",
	"kk": "kaz", "kl": "kal", "km": "khm", "kn": "kan", "ko": "kor", "kr": "kau", "ks": "kas", "ku": "kur", "kv": "kom", "kw": "cor",
	"ky": "kir", "la": "lat", "lb": "ltz", "lg": "lug", "li": "lim", "ln": "lin", "lo": "lao", "lt": "lit", "lu": "lub", "lv": "lav",
	"mg": "mlg", "mh": "mah", "mi": "mri", "mk": "mkd", "ml": "mal", "mn": "mon", "mr": "mar", "ms": "msa", "mt": "mlt", "my": "mya",
	"na": "nau", "nb": "nob", "nd": "nde", "ne": "nep", "ng": "ndo", "nl": "nld", "nn": "nno", "no": "nor", "nr": "nbl", "nv": "nav",
	"ny": "nya", "oc": "oci", "oj": "oji", "om": "orm", "or": "ori", "os": "oss", "pa": "pan", "pi": "pli", "pl": "pol", "ps": "pus",
	"pt": "por", "qu": "que", "rm": "roh", "rn": "run", "ro": "ron", "ru": "rus", "rw": "kin", "sa": "san", "sc": "srd", "sd": "snd",
	"se": "sme", "sg": "sag", "si": "sin", "sk": "slk", "sl": "slv", "sm": "smo", "sn": "sna", "so": "som", "sq": "sqi", "sr": "srp",
	"ss": "ssw", "st": "sot", "su": "sun", "sv": "swe", "sw": "swa", "ta": "tam", "te": "tel", "tg": "tgk", "th": "tha", "ti": "tir",
	"tk": "tuk", "tl": "tgl", "tn": "tsn", "to": "ton", "tr": "tur", "ts": "tso", "tt": "tat", "tw": "twi", "ty": "tah", "ug": "uig",
	"uk": "ukr", "ur": "urd", "uz": "uzb", "ve": "ven", "vi": "vie", "vo": "vol", "wa": "wln", "wo": "wol", "xh": "xho", "yi": "yid",
	"yo": "yor", "za": "zha", "zh": "zho", "zu": "zul",
}

// iso639B lists the bibliographic codes that differ from the terminology ones (T → B).
var iso639B = map[string]string{
	"sqi": "alb", "hye": "arm", "eus": "baq", "mya": "bur", "zho": "chi", "ces": "cze", "nld": "dut", "fra": "fre", "kat": "geo", "deu": "ger",
	"ell": "gre", "isl": "ice", "mkd": "mac", "mri": "mao", "msa": "may", "fas": "per", "ron": "rum", "slk": "slo", "bod": "tib", "cym": "wel",
}

// lang639 turns a stream's language tag into an ISO 639-2 code (T for MP4, B for Matroska);
// "und" when there is none or it isn't known.
func lang639(tag string, bibliographic bool) string {
	p, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(tag)), "-")
	p, _, _ = strings.Cut(p, "_")
	code := ""
	switch len(p) {
	case 2:
		code = iso639_1[p]
	case 3:
		code = p
		for t, b := range iso639B { // a B code given where T is wanted, or the other way round
			if p == b {
				code = t
			}
		}
	}
	if code == "" || strings.Trim(code, "abcdefghijklmnopqrstuvwxyz") != "" {
		return "und"
	}
	if bibliographic {
		if b, ok := iso639B[code]; ok {
			return b
		}
	}
	return code
}

// sameLang reports whether two language tags name the same language (the region aside).
func sameLang(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return lang639(a, false) == lang639(b, false) && lang639(a, false) != "und"
}
