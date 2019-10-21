package transeq

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"reflect"

	"github.com/Ayllonbe/gotranseq/ncbicode"
)

// Options struct to store required command line args
type Options struct {
	Frame       string `short:"f" long:"frame" value-name:"<code>" description:"Frame to translate. Possible values:\n  [1, 2, 3, F, -1, -2, -3, R, 6]\n F: forward three frames\n R: reverse three frames\n 6: all 6 frames\n" default:"1"`
	Table       int    `short:"t" long:"table" value-name:"<code>" description:"NCBI code to use, see https://www.ncbi.nlm.nih.gov/Taxonomy/Utils/wprintgc.cgi?chapter=tgencodes#SG1 for details. Available codes: \n 0: Standard code\n 2: The Vertebrate Mitochondrial Code\n 3: The Yeast Mitochondrial Code\n 4: The Mold, Protozoan, and Coelenterate Mitochondrial Code and the Mycoplasma/Spiroplasma Code\n 5: The Invertebrate Mitochondrial Code\n 6: The Ciliate, Dasycladacean and Hexamita Nuclear Code\n 9: The Echinoderm and Flatworm Mitochondrial Code\n 10: The Euplotid Nuclear Code\n 11: The Bacterial, Archaeal and Plant Plastid Code\n 12: The Alternative Yeast Nuclear Code\n 13: The Ascidian Mitochondrial Code\n 14: The Alternative Flatworm Mitochondrial Code\n16: Chlorophycean Mitochondrial Code\n 21: Trematode Mitochondrial Code\n22: Scenedesmus obliquus Mitochondrial Code\n 23: Thraustochytrium Mitochondrial Code\n 24: Pterobranchia Mitochondrial Code\n 25: Candidate Division SR1 and Gracilibacteria Code\n 26: Pachysolen tannophilus Nuclear Code\n 29: Mesodinium Nuclear\n 30: Peritrich Nuclear\n" default:"0"`
	Clean       bool   `short:"c" long:"clean" description:"Replace stop codon '*' by 'X'"`
	Alternative bool   `short:"a" long:"alternative" description:"Define frame '-1' as using the set of codons starting with the last codon of the sequence"`
	Trim        bool   `short:"T" long:"trim" description:"Removes all 'X' and '*' characters from the right end of the translation. The trimming process starts at the end and continues until the next character is not a 'X' or a '*'"`
	NumWorker   int    `short:"n" long:"numcpu" value-name:"<n>" description:"Number of threads to use, default is number of CPU"`
}

const (
	// nCode has to be 0 in order to compute two-letters code
	nCode uint8 = iota
	aCode
	cCode
	tCode
	gCode
	uCode = tCode

	// Length of the array to store codon <-> AA correspondance
	// uses gCode because it's the biggest uint8 of all codes
	arrayCodeSize = (uint32(gCode) | uint32(gCode)<<8 | uint32(gCode)<<16) + 1
)

var letterCode = map[byte]uint8{
	'A': aCode,
	'C': cCode,
	'T': tCode,
	'G': gCode,
	'N': nCode,
	'U': uCode,
}

func createCodeArray(tableCode int, clean bool) ([arrayCodeSize]byte, error) {

	var codes [arrayCodeSize]byte
	for i := range codes {
		codes[i] = unknown
	}

	twoLetterMap := map[string][]byte{}
	codeMap, err := ncbicode.LoadTableCode(tableCode)
	if err != nil {
		return codes, err
	}

	for codon, aaCode := range codeMap {

		if !(clean && aaCode == stop) {
			// codon is always a 3 char string, for example 'ACG'
			// each  nucleotide of the codon is represented by an uint8
			n1, n2, n3 := letterCode[codon[0]], letterCode[codon[1]], letterCode[codon[2]]
			index := uint32(n1) | uint32(n2)<<8 | uint32(n3)<<16
			codes[index] = aaCode
		}
		// in some case, all codon for an AA will start with the same
		// two nucleotid, for example:
		// GTC -> 'V'
		// GTG -> 'V'
		aaCodeArray, ok := twoLetterMap[codon[:2]]
		if !ok {
			twoLetterMap[codon[:2]] = []byte{aaCode}
		} else {
			if aaCode != aaCodeArray[0] {
				twoLetterMap[codon[:2]] = append(aaCodeArray, aaCode)
			}
		}
	}

	for twoLetterCodon, aaCodeArray := range twoLetterMap {

		aaCode := aaCodeArray[0]
		if len(aaCodeArray) == 1 && !(clean && aaCode == stop) {

			n1, n2 := letterCode[twoLetterCodon[0]], letterCode[twoLetterCodon[1]]
			index := uint32(n1) | uint32(n2)<<8
			codes[index] = aaCode
		}
	}
	return codes, nil
}

func computeFrames(frameName string) (frames [6]int, reverse bool, err error) {

	var frameMap = map[string]struct {
		frames  [6]int
		reverse bool
	}{
		"1":  {[6]int{1, 0, 0, 0, 0, 0}, false},
		"2":  {[6]int{0, 1, 0, 0, 0, 0}, false},
		"3":  {[6]int{0, 0, 1, 0, 0, 0}, false},
		"F":  {[6]int{1, 1, 1, 0, 0, 0}, false},
		"-1": {[6]int{0, 0, 0, 1, 0, 0}, true},
		"-2": {[6]int{0, 0, 0, 0, 1, 0}, true},
		"-3": {[6]int{0, 0, 0, 0, 0, 1}, true},
		"R":  {[6]int{0, 0, 0, 1, 1, 1}, true},
		"6":  {[6]int{1, 1, 1, 1, 1, 1}, true},
	}

	f, ok := frameMap[frameName]
	if !ok {
		return frames, false, fmt.Errorf("wrong value for -f | --frame parameter: %s", frameName)
	}
	return f.frames, f.reverse, nil
}

// Translate read a fata file and translate each sequence to the corresponding prot sequence
// with the specified options
func Translate(inputSequence io.Reader, out io.Writer, options Options) error {

	framesToGenerate, reverse, err := computeFrames(options.Frame)
	if err != nil {
		return err
	}

	codes, err := createCodeArray(options.Table, options.Clean)
	if err != nil {
		return err
	}

	fnaSequences := make(chan encodedSequence, 100)
	errs := make(chan error, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(options.NumWorker)

	for nWorker := 0; nWorker < options.NumWorker; nWorker++ {

		go func() {

			defer wg.Done()

			w := newWriter(codes, framesToGenerate, reverse, options.Alternative, options.Trim)
    		
			for sequence := range fnaSequences {

				select {
				case <-ctx.Done():
					return
				default:
				}
				
				w.translate(sequence)

				if len(w.buf) > maxBufferSize {
					w.flush(out, cancel, errs)
				}
				pool.Put(sequence)
			}
			w.flush(out, cancel, errs)
		}()
	}
	readSequenceFromFasta(ctx, inputSequence, fnaSequences)

	wg.Wait()

	select {
	case err, ok := <-errs:
		if ok {
			return err
		}
	default:
	}
	return nil
}

// fasta format is:
//
// >sequenceID some comments on sequence
// ACAGGCAGAGACACGACAGACGACGACACAGGAGCAGACAGCAGCAGACGACCACATATT
// TTTGCGGTCACATGACGACTTCGGCAGCGA
//
// see https://blast.ncbi.nlm.nih.gov/Blast.cgi?CMD=Web&PAGE_TYPE=BlastDocs&DOC_TYPE=BlastHelp
// section 1 for details
func readSequenceFromFasta(ctx context.Context, inputSequence io.Reader, fnaSequences chan encodedSequence) {

	scanner := bufio.NewScanner(inputSequence)
	buf := bytes.NewBuffer(make([]byte, 0, 512))
	headerSize := 0

Loop:
	for scanner.Scan() {

		line := scanner.Bytes()
		
		fmt.Println(line)
		if len(line) == 0 {
			continue
		}
		if line[0] == '>' {
			if buf.Len() > 0 {
				select {
				case <-ctx.Done():
					break Loop
				default:
				}
				fnaSequences <- newEncodedSequence(buf, headerSize)
			}
			buf.Reset()
			headerSize = len(line)
		}
		buf.Write(line)
	}

	fnaSequences <- newEncodedSequence(buf, headerSize)

	close(fnaSequences)
}
