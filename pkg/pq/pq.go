package pq

type EarlyPriorityQueue []*EarlyPacket

type EarlyPacket struct {
	Priority uint32
	Index    int
	Payload  []byte
}

// https://pkg.go.dev/container/heap#example-package-PriorityQueue

func (pq EarlyPriorityQueue) Len() int { return len(pq) }

func (pq EarlyPriorityQueue) Less(i, j int) bool {
	return pq[i].Priority < pq[j].Priority
}

func (pq EarlyPriorityQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].Index = i
	pq[j].Index = j
}

func (pq *EarlyPriorityQueue) Push(x any) {
	n := len(*pq)
	item := x.(*EarlyPacket)
	item.Index = n
	*pq = append(*pq, item)
}

func (pq *EarlyPriorityQueue) Pop() any {
	old := *pq
	n := len(old)
	item := old[n-1]
	old[n-1] = nil  // avoid memory leak
	item.Index = -1 // for safety
	*pq = old[0 : n-1]
	return item
}
