package main

import (
	"bytes"
	"encoding/base64"
	"math/rand/v2"
	"testing"
)

// QuickXorHash test vectors. Microsoft publishes the algorithm (as C#) but no vectors; these are the
// ones rclone checks its port against (backend/onedrive/quickxorhash), inputs and digests in base64.
var quickXorVectors = []struct{ in, out string }{
	{``, "AAAAAAAAAAAAAAAAAAAAAAAAAAA="},
	{`Sg==`, "SgAAAAAAAAAAAAAAAQAAAAAAAAA="},
	{`tbQ=`, "taAFAAAAAAAAAAAAAgAAAAAAAAA="},
	{`0pZP`, "0rDEEwAAAAAAAAAAAwAAAAAAAAA="},
	{`jRRDVA==`, "jaDAEKgAAAAAAAAABAAAAAAAAAA="},
	{`eAV52qE=`, "eChAHrQRCgAAAAAABQAAAAAAAAA="},
	{`luBZlaT6`, "lgBHFipBCn0AAAAABgAAAAAAAAA="},
	{`qaApEj66lw==`, "qQBFCiTgA11cAgAABwAAAAAAAAA="},
	{`/aNzzCFPS/A=`, "/RjFHJgRgicsAR4ACAAAAAAAAAA="},
	{`n6Neh7p6fFgm`, "nxiFFw6hCz3wAQsmCQAAAAAAAAA="},
	{`J9iPGCbfZSTNyw==`, "J8DGIzBggm+UgQTNUgYAAAAAAAA="},
	{`i+UZyUGJKh+ISbk=`, "iyhHBpIRhESo4AOIQ0IuAAAAAAA="},
	{`h490d57Pqz5q2rtT`, "h3gEHe7giWeswgdq3MYupgAAAAA="},
	{`vPgoDjOfO6fm71RxLw==`, "vMAHChwwg0/s4BTmdQcV4vACAAA="},
	{`XoJ1AsoR4fDYJrDqYs4=`, "XhBEHQSgjAiEAx7YPgEs1CEGZwA="},
	{`gQaybEqS/4UlDc8e4IJm`, "gDCALNigBEn8oxAlZ8AzPAAOQZg="},
	{`2fuxhBJXtpWFe8dOfdGeHw==`, "O9tHLAghgSvYohKFyMMxnNCHaHg="},
	{`XBV6YKU9V7yMakZnFIxIkuU=`, "HbplHsBQih5cgReMQYMRzkABRiA="},
	{`XJZSOiNO2bmfKnTKD7fztcQX`, "/6ZArHQwAidkIxefQgEdlPGAW8w="},
	{`g8VtAh+2Kf4k0kY5tzji2i2zmA==`, "wDNrgwHWAVukwB8kg4YRcnALHIg="},
	{`T6LYJIfDh81JrAK309H2JMJTXis=`, "zBTHrspn3mEcohlJdIUAbjGNaNg="},
	{`DWAAX5/CIfrmErgZa8ot6ZraeSbu`, "LR2Z0PjuRYGKQB/mhQAuMrAGZbQ="},
	{`N9abi3qy/mC1THZuVLHPpx7SgwtLOA==`, "1KTYttCBEen8Hwy1doId3ECFWDw="},
	{`LlUe7wHerLqEtbSZLZgZa9u0m7hbiFs=`, "TqVZpxs3cN61BnuFvwUtMtECTGQ="},
	{`bU2j/0XYdgfPFD4691jV0AOUEUPR4Z5E`, "bnLBiLpVgnxVkXhNsIAPdHAPLFQ="},
	{`lScPwPsyUsH2T1Qsr31wXtP55Wqbe47Uyg==`, "VDMSy8eI26nBHCB0e8gVWPCKPsA="},
	{`rJaKh1dLR1k+4hynliTZMGf8Nd4qKKoZiAM=`, "r7bjwkl8OYQeNaMcCY8fTmEJEmQ="},
	{`pPsT0CPmHrd3Frsnva1pB/z1ytARLeHEYRCo`, "Rdg7rCcDomL59pL0s6GuTvqLVqQ="},
	{`wSRChaqmrsnMrfB2yqI43eRWbro+f9kBvh+01w==`, "YTtloIi6frI7HX3vdLvE7I2iUOA="},
	{`apL67KMIRxQeE9k1/RuW09ppPjbF1WeQpTjSWtI=`, "CIpedls+ZlSQ654fl+X26+Q7LVU="},
	{`53yx0/QgMTVb7OOzHRHbkS7ghyRc+sIXxi7XHKgT`, "zfJtLGFgR9DB3Q64fAFIp+S5iOY="},
	{`PwXNnutoLLmxD8TTog52k8cQkukmT87TTnDipKLHQw==`, "PTaGs7yV3FUyBy/SfU6xJRlCJlI="},
	{`NbYXsp5/K6mR+NmHwExjvWeWDJFnXTKWVlzYHoesp2E=`, "wjuAuWDiq04qDt1R8hHWDDcwVoQ="},
	{`qQ70RB++JAR5ljNv3lJt1PpqETPsckopfonItu18Cr3E`, "FkJaeg/0Z5+euShYlLpE2tJh+Lo="},
	{`RhzSatQTQ9/RFvpHyQa1WLdkr3nIk6MjJUma998YRtp44A==`, "SPN2D29reImAqJezlqV2DLbi8tk="},
	{`DND1u1uZ5SqZVpRUk6NxSUdVo7IjjL9zs4A1evDNCDLcXWc=`, "S6lBk2hxI2SWBfn7nbEl7D19UUs="},
	{`jEi62utFz69JMYHjg1iXy7oO6ZpZSLcVd2B+pjm6BGsv/CWi`, "s0lYU9tr/bp9xsnrrjYgRS5EvV8="},
	{`hfS3DZZnhy0hv7nJdXLv/oJOtIgAuP9SInt/v8KeuO4/IvVh4A==`, "CV+HQCdd2A/e/vdi12f2UU55GLA="},
	{`EkPQAC6ymuRrYjIXD/LT/4Vb+7aTjYVZOHzC8GPCEtYDP0+T3Nc=`, "kE9H9sEmr3vHBYUiPbvsrcDgSEo="},
	{`vtBOGIENG7yQ/N7xNWPNIgy66Gk/I2Ur/ZhdFNUK9/1FCZuu/KeS`, "+Fgp3HBimtCzUAyiinj3pkarYTk="},
	{`YnF4smoy9hox2jBlJ3VUa4qyCRhOZbWcmFGIiszTT4zAdYHsqJazyg==`, "arkIn+ELddmE8N34J9ydyFKW+9w="},
	{`0n7nl3YJtipy6yeUbVPWtc2h45WbF9u8hTz5tNwj3dZZwfXWkk+GN3g=`, "YJLNK7JR64j9aODWfqDvEe/u6NU="},
	{`FnIIPHayc1pHkY4Lh8+zhWwG8xk6Knk/D3cZU1/fOUmRAoJ6CeztvMOL`, "22RPOylMtdk7xO/QEQiMli4ql0k="},
	{`J82VT7ND0Eg1MorSfJMUhn+qocF7PsUpdQAMrDiHJ2JcPZAHZ2nyuwjoKg==`, "pOR5eYfwCLRJbJsidpc1rIJYwtM="},
	{`Zbu+78+e35ZIymV5KTDdub5McyI3FEO8fDxs62uWHQ9U3Oh3ZqgaZ30SnmQ=`, "DbvbTkgNTgWRqRidA9r1jhtUjro="},
	{`lgybK3Da7LEeY5aeeNrqcdHvv6mD1W4cuQ3/rUj2C/CNcSI0cAMw6vtpVY3y`, "700RQByn1lRQSSme9npQB/Ye+bY="},
	{`jStZgKHv4QyJLvF2bYbIUZi/FscHALfKHAssTXkrV1byVR9eACwW9DNZQRHQwg==`, "uwN55He8xgE4g93dH9163xPew4U="},
	{`V1PSud3giF5WW72JB/bgtltsWtEB5V+a+wUALOJOGuqztzVXUZYrvoP3XV++gM0=`, "U+3ZfUF/6mwOoHJcSHkQkckfTDA="},
	{`VXs4t4tfXGiWAL6dlhEMm0YQF0f2w9rzX0CvIVeuW56o6/ec2auMpKeU2VeteEK5`, "sq24lSf7wXLH8eigHl07X+qPTps="},
	{`bLUn3jLH+HFUsG3ptWTHgNvtr3eEv9lfKBf0jm6uhpqhRwtbEQ7Ovj/hYQf42zfdtQ==`, "uC8xrnopGiHebGuwgq607WRQyxQ="},
	{`4SVmjtXIL8BB8SfkbR5Cpaljm2jpyUfAhIBf65XmKxHlz9dy5XixgiE/q1lv+esZW/E=`, "wxZ0rxkMQEnRNAp8ZgEZLT4RdLM="},
	{`pMljctlXeFUqbG3BppyiNbojQO3ygg6nZPeUZaQcVyJ+Clgiw3Q8ntLe8+02ZSfyCc39`, "aZEPmNvOXnTt7z7wt+ewV7QGMlg="},
	{`C16uQlxsHxMWnV2gJhFPuJ2/guZ4N1YgmNvAwL1yrouGQtwieGx8WvZsmYRnX72JnbVtTw==`, "QtlSNqXhVij64MMhKJ3EsDFB/z8="},
	{`7ZVDOywvrl3L0GyKjjcNg2CcTI81n2CeUbzdYWcZOSCEnA/xrNHpiK01HOcGh3BbxuS4S6g=`, "4NznNJc4nmXeApfiCFTq/H5LbHw="},
	{`JXm2tTVqpYuuz2Cc+ZnPusUb8vccPGrzWK2oVwLLl/FjpFoxO9FxGlhnB08iu8Q/XQSdzHn+`, "IwE5+2pKNcK366I2k2BzZYPibSI="},
	{`TiiU1mxzYBSGZuE+TX0l9USWBilQ7dEml5lLrzNPh75xmhjIK8SGqVAkvIMgAmcMB+raXdMPZg==`, "yECGHtgR128ScP4XlvF96eLbIBE="},
	{`zz+Q4zi6wh0fCJUFU9yUOqEVxlIA93gybXHOtXIPwQQ44pW4fyh6BRgc1bOneRuSWp85hwlTJl8=`, "+3Ef4D6yuoC8J+rbFqU1cegverE="},
	{`sa6SHK9z/G505bysK5KgRO2z2cTksDkLoFc7sv0tWBmf2G2mCiozf2Ce6EIO+W1fRsrrtn/eeOAV`, "xZg1CwMNAjN0AIXw2yh4+1N3oos="},
	{`0qx0xdyTHhnKJ22IeTlAjRpWw6y2sOOWFP75XJ7cleGJQiV2kyrmQOST4DGHIL0qqA7sMOdzKyTViw==`, "bS0tRYPkP1Gfc+ZsBm9PMzPunG8="},
	{`QuzaF0+5ooig6OLEWeibZUENl8EaiXAQvK9UjBEauMeuFFDCtNcGs25BDtJGGbX90gH4VZvCCDNCq4s=`, "rggokuJq1OGNOfB6aDp2g4rdPgw="},
	{`+wg2x23GZQmMLkdv9MeAdettIWDmyK6Wr+ba23XD+Pvvq1lIMn9QIQT4Z7QHJE3iC/ZMFgaId9VAyY3d`, "ahQbTmOdiKUNdhYRHgv5/Ky+Y6k="},
	{`y0ydRgreRQwP95vpNP92ioI+7wFiyldHRbr1SfoPNdbKGFA0lBREaBEGNhf9yixmfE+Azo2AuROxb7Yc7g==`, "cJKFc0dXfiN4hMg1lcMf5E4gqvo="},
	{`LxlVvGXSQlSubK8r0pGf9zf7s/3RHe75a2WlSXQf3gZFR/BtRnR7fCIcaG//CbGfodBFp06DBx/S9hUV8Bk=`, "NwuwhhRWX8QZ/vhWKWgQ1+rNomI="},
	{`L+LSB8kmGMnHaWVA5P/+qFnfQliXvgJW7d2JGAgT6+koi5NQujFW1bwQVoXrBVyob/gBxGizUoJMgid5gGNo`, "ndX/KZBtFoeO3xKeo1ajO/Jy+rY="},
	{`Mb7EGva2rEE5fENDL85P+BsapHEEjv2/siVhKjvAQe02feExVOQSkfmuYzU/kTF1MaKjPmKF/w+cbvwfdWL8aQ==`, "n1anP5NfvD4XDYWIeRPW3ZkPv1Y="},
	{`jyibxJSzO6ZiZ0O1qe3tG/bvIAYssvukh9suIT5wEy1JBINVgPiqdsTW0cOpP0aUfP7mgqLfADkzI/m/GgCuVhr8oFLrOCoTx1/psBOWwhltCbhUx51Icm9aH8tY4Z3ccU+6BKpYQkLCy0B/A9Zc`, "hZfLIilSITC6N3e3tQ/iSgEzkto="},
	{`ikwCorI7PKWz17EI50jZCGbV9JU2E8bXVfxNMg5zdmqSZ2NlsQPp0kqYIPjzwTg1MBtfWPg53k0h0P2naJNEVgrqpoHTfV2b3pJ4m0zYPTJmUX4Bg/lOxcnCxAYKU29Y5F0U8Quz7ZXFBEweftXxJ7RS4r6N7BzJrPsLhY7hgck=`, "imAoFvCWlDn4yVw3/oq1PDbbm6U="},
	{`PfxMcUd0vIW6VbHG/uj/Y0W6qEoKmyBD0nYebEKazKaKG+UaDqBEcmQjbfQeVnVLuodMoPp7P7TR1htX5n2VnkHh22xDyoJ8C/ZQKiSNqQfXvh83judf4RVr9exJCud8Uvgip6aVZTaPrJHVjQhMCp/dEnGvqg0oN5OVkM2qqAXvA0teKUDhgNM71sDBVBCGXxNOR2bpbD1iM4dnuT0ey4L+loXEHTL0fqMeUcEi2asgImnlNakwenDzz0x57aBwyq3AspCFGB1ncX4yYCr/OaCcS5OKi/00WH+wNQU3`, "QX/YEpG0gDsmhEpCdWhsxDzsfVE="},
	{`qwGf2ESubE5jOUHHyc94ORczFYYbc2OmEzo+hBIyzJiNwAzC8PvJqtTzwkWkSslgHFGWQZR2BV5+uYTrYT7HVwRM40vqfj0dBgeDENyTenIOL1LHkjtDKoXEnQ0mXAHoJ8PjbNC93zi5TovVRXTNzfGEs5dpWVqxUzb5lc7dwkyvOluBw482mQ4xrzYyIY1t+//OrNi1ObGXuUw2jBQOFfJVj2Y6BOyYmfB1y36eBxi3zxeG5d5NYjm2GSh6e08QMAwu3zrINcqIzLOuNIiGXBtl7DjKt7b5wqi4oFiRpZsCyx2smhSrdrtK/CkdU6nDN+34vSR/M8rZpWQdBE7a8g==`, "WYT9JY3JIo/pEBp+tIM6Gt2nyTM="},
	{`w0LGhqU1WXFbdavqDE4kAjEzWLGGzmTNikzqnsiXHx2KRReKVTxkv27u3UcEz9+lbMvYl4xFf2Z4aE1xRBBNd1Ke5C0zToSaYw5o4B/7X99nKK2/XaUX1byLow2aju2XJl2OpKpJg+tSJ2fmjIJTkfuYUz574dFX6/VXxSxwGH/xQEAKS5TCsBK3CwnuG1p5SAsQq3gGVozDWyjEBcWDMdy8/AIFrj/y03Lfc/RNRCQTAfZbnf2QwV7sluw4fH3XJr07UoD0YqN+7XZzidtrwqMY26fpLZnyZjnBEt1FAZWO7RnKG5asg8xRk9YaDdedXdQSJAOy6bWEWlABj+tVAigBxavaluUH8LOj+yfCFldJjNLdi90fVHkUD/m4Mr5OtmupNMXPwuG3EQlqWUVpQoYpUYKLsk7a5Mvg6UFkiH596y5IbJEVCI1Kb3D1`, "e3+wo77iKcILiZegnzyUNcjCdoQ="},
}

func TestQuickXorHashVectors(t *testing.T) {
	for i, v := range quickXorVectors {
		in, _ := base64.StdEncoding.DecodeString(v.in)
		for _, step := range []int{len(in) + 1, 1, 3, 7, 64, 161} { // whole, and in pieces of every size
			h := newQuickXor()
			for p := 0; p < len(in); p += step {
				h.Write(in[p:min(p+step, len(in))])
			}
			if got := base64.StdEncoding.EncodeToString(h.Sum(nil)); got != v.out {
				t.Fatalf("vector %d (%d bytes, pieces of %d): %s, want %s", i, len(in), step, got, v.out)
			}
		}
	}
}

// naiveQuickXor is the algorithm as its description reads, a bit at a time: byte i goes into the
// 160-bit ring at bit (11·i) mod 160, then the length is XORed into the last 8 bytes.
func naiveQuickXor(p []byte) []byte {
	var ring [160]bool
	for i, b := range p {
		at := (i * 11) % 160
		for k := 0; k < 8; k++ {
			if b>>k&1 == 1 {
				ring[(at+k)%160] = !ring[(at+k)%160]
			}
		}
	}
	out := make([]byte, 20)
	for bit, on := range ring {
		if on {
			out[bit/8] |= 1 << (bit % 8)
		}
	}
	for k := 0; k < 8; k++ {
		out[12+k] ^= byte(uint64(len(p)) >> (8 * k))
	}
	return out
}

func TestQuickXorHashMatchesItsDescription(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for _, n := range []int{0, 1, 19, 20, 21, 159, 160, 161, 1000, 1 << 16} {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(r.Uint32())
		}
		h := newQuickXor()
		for p := 0; p < n; p += 333 {
			h.Write(b[p:min(p+333, n)])
		}
		if got, want := h.Sum(nil), naiveQuickXor(b); !bytes.Equal(got, want) {
			t.Fatalf("%d bytes: %x, want %x", n, got, want)
		}
	}
}
