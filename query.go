package chromem

import (
	"cmp"
	"container/heap"
	"context"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
)

type docSim struct {
	docID      string
	similarity float32
}

// docMaxHeap is a max-heap of docSims, based on similarity.
// See https://pkg.go.dev/container/heap@go1.22#example-package-IntHeap
type docMaxHeap []docSim

func (h docMaxHeap) Len() int           { return len(h) }
func (h docMaxHeap) Less(i, j int) bool { return h[i].similarity < h[j].similarity }
func (h docMaxHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

func (h *docMaxHeap) Push(x any) {
	// Push and Pop use pointer receivers because they modify the slice's length,
	// not just its contents.
	*h = append(*h, x.(docSim))
}

func (h *docMaxHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[0 : n-1]
	return x
}

// maxDocSims manages a max-heap of docSims with a fixed size, keeping the n highest
// similarities. It's safe for concurrent use, but not the result of values().
// In our benchmarks this was faster than sorting a slice of docSims at the end.
type maxDocSims struct {
	h    docMaxHeap
	lock sync.RWMutex
	size int
}

// newMaxDocSims creates a new nMaxDocs with a fixed size.
func newMaxDocSims(size int) *maxDocSims {
	return &maxDocSims{
		h:    make(docMaxHeap, 0, size),
		size: size,
	}
}

// add inserts a new docSim into the heap, keeping only the top n similarities.
func (d *maxDocSims) add(doc docSim) {
	d.lock.Lock()
	defer d.lock.Unlock()
	if d.h.Len() < d.size {
		heap.Push(&d.h, doc)
	} else if d.h.Len() > 0 && d.h[0].similarity < doc.similarity {
		// Replace the smallest similarity if the new doc's similarity is higher
		heap.Pop(&d.h)
		heap.Push(&d.h, doc)
	}
}

// values returns the docSims in the heap, sorted by similarity (descending).
// The call itself is safe for concurrent use with add(), but the result isn't.
// Only work with the result after all calls to add() have finished.
func (d *maxDocSims) values() []docSim {
	d.lock.RLock()
	defer d.lock.RUnlock()
	slices.SortFunc(d.h, func(i, j docSim) int {
		return cmp.Compare(j.similarity, i.similarity)
	})
	return d.h
}

// filterDocs filters a map of documents by metadata and content.
// It does this concurrently.
func filterDocs(docs map[string]*Document, where map[string]string, whereDocuments []WhereDocument) []*Document {
	filteredDocs := make([]*Document, 0, len(docs))
	filteredDocsLock := sync.Mutex{}

	// Determine concurrency. Use number of docs or CPUs, whichever is smaller.
	numCPUs := runtime.NumCPU()
	numDocs := len(docs)
	concurrency := numCPUs
	if numDocs < numCPUs {
		concurrency = numDocs
	}

	docChan := make(chan *Document, concurrency*2)

	wg := sync.WaitGroup{}
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for doc := range docChan {
				if documentMatchesFilters(doc, where, whereDocuments) {
					filteredDocsLock.Lock()
					filteredDocs = append(filteredDocs, doc)
					filteredDocsLock.Unlock()
				}
			}
		}()
	}

	for _, doc := range docs {
		docChan <- doc
	}
	close(docChan)

	wg.Wait()

	// With filteredDocs being initialized as potentially large slice, let's return
	// nil instead of the empty slice.
	if len(filteredDocs) == 0 {
		filteredDocs = nil
	}
	return filteredDocs
}

type WhereDocumentOperator string

const (
	WhereDocumentOperatorEquals      WhereDocumentOperator = "$eq"
	WhereDocumentOperatorContains    WhereDocumentOperator = "$contains"
	WhereDocumentOperatorNotContains WhereDocumentOperator = "$not_contains"
	WhereDocumentOperatorOr          WhereDocumentOperator = "$or"
	WhereDocumentOperatorAnd         WhereDocumentOperator = "$and"
)

type WhereDocument struct {
	Operator       WhereDocumentOperator
	Value          string
	WhereDocuments []WhereDocument
}

func (wd *WhereDocument) Validate() error {

	if !slices.Contains([]WhereDocumentOperator{WhereDocumentOperatorContains, WhereDocumentOperatorNotContains, WhereDocumentOperatorOr, WhereDocumentOperatorAnd}, wd.Operator) {
		return fmt.Errorf("unsupported where document operator %s", wd.Operator)
	}

	if wd.Operator == "" {
		return fmt.Errorf("where document operator is empty")
	}

	// $eq, $contains and $not_contains require a string value
	if slices.Contains([]WhereDocumentOperator{WhereDocumentOperatorEquals, WhereDocumentOperatorContains, WhereDocumentOperatorNotContains}, wd.Operator) {
		if wd.Value == "" {
			return fmt.Errorf("where document operator %s requires a value", wd.Operator)
		}
	}

	// $or requires sub-filters
	if slices.Contains([]WhereDocumentOperator{WhereDocumentOperatorOr, WhereDocumentOperatorAnd}, wd.Operator) {
		if len(wd.WhereDocuments) == 0 {
			return fmt.Errorf("where document operator %s must have at least one sub-filter", wd.Operator)
		}
	}

	for _, wd := range wd.WhereDocuments {
		if err := wd.Validate(); err != nil {
			return err
		}
	}

	return nil
}

// Matches checks if a document matches the WhereDocument filter(s)
// There is no error checking on the WhereDocument struct, so it must be validated before calling this function.
func (wd *WhereDocument) Matches(doc *Document) bool {
	switch wd.Operator {
	case WhereDocumentOperatorEquals:
		return doc.Content == wd.Value
	case WhereDocumentOperatorContains:
		return strings.Contains(doc.Content, wd.Value)
	case WhereDocumentOperatorNotContains:
		return !strings.Contains(doc.Content, wd.Value)
	case WhereDocumentOperatorOr:
		for _, subFilter := range wd.WhereDocuments {
			if subFilter.Matches(doc) {
				return true
			}
		}
		return false
	case WhereDocumentOperatorAnd:
		for _, subFilter := range wd.WhereDocuments {
			if !subFilter.Matches(doc) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// documentMatchesFilters checks if a document matches the given filters.
// When calling this function, the whereDocument structs must already be validated!
func documentMatchesFilters(document *Document, where map[string]string, whereDocuments []WhereDocument) bool {
	// A document's metadata must have *all* the fields in the where clause.
	for k, v := range where {
		// TODO: Do we want to check for existence of the key? I.e. should
		// a where clause with empty string as value match a document's
		// metadata that doesn't have the key at all?
		if document.Metadata[k] != v {
			return false
		}
	}

	// A document must satisfy *all* WhereDocument filters (that's basically a top-level $and operator)
	for _, whereDocument := range whereDocuments {
		if !whereDocument.Matches(document) {
			return false
		}
	}

	return true
}

func getMostSimilarDocs(ctx context.Context, queryVectors, negativeVector []float32, negativeFilterThreshold float32, docs []*Document, n int) ([]docSim, error) {
	nMaxDocs := newMaxDocSims(n)

	// Determine concurrency. Use number of docs or CPUs, whichever is smaller.
	numCPUs := runtime.NumCPU()
	numDocs := len(docs)
	concurrency := numCPUs
	if numDocs < numCPUs {
		concurrency = numDocs
	}

	var sharedErr error
	sharedErrLock := sync.Mutex{}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	setSharedErr := func(err error) {
		sharedErrLock.Lock()
		defer sharedErrLock.Unlock()
		// Another goroutine might have already set the error.
		if sharedErr == nil {
			sharedErr = err
			// Cancel the operation for all other goroutines.
			cancel(sharedErr)
		}
	}

	wg := sync.WaitGroup{}
	// Instead of using a channel to pass documents into the goroutines, we just
	// split the slice into sub-slices and pass those to the goroutines.
	// This turned out to be faster in the query benchmarks.
	subSliceSize := len(docs) / concurrency // Can leave remainder, e.g. 10/3 = 3; leaves 1
	rem := len(docs) % concurrency
	for i := 0; i < concurrency; i++ {
		start := i * subSliceSize
		end := start + subSliceSize
		// Add remainder to last goroutine
		if i == concurrency-1 {
			end += rem
		}

		wg.Add(1)
		go func(subSlice []*Document) {
			defer wg.Done()
			for _, doc := range subSlice {
				// Stop work if another goroutine encountered an error.
				if ctx.Err() != nil {
					return
				}

				// As the vectors are normalized, the dot product is the cosine similarity.
				sim, err := dotProduct(queryVectors, doc.Embedding)
				if err != nil {
					setSharedErr(fmt.Errorf("couldn't calculate similarity for document '%s': %w", doc.ID, err))
					return
				}

				if negativeFilterThreshold > 0 {
					nsim, err := dotProduct(negativeVector, doc.Embedding)
					if err != nil {
						setSharedErr(fmt.Errorf("couldn't calculate negative similarity for document '%s': %w", doc.ID, err))
						return
					}

					if nsim > negativeFilterThreshold {
						continue
					}
				}

				nMaxDocs.add(docSim{docID: doc.ID, similarity: sim})
			}
		}(docs[start:end])
	}

	wg.Wait()

	if sharedErr != nil {
		return nil, sharedErr
	}

	return nMaxDocs.values(), nil
}
