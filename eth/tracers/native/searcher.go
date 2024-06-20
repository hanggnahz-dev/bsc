package native

import (
	"fmt"
	"reflect"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

type funcSelector [4]byte
type FuncSelector hexutil.Bytes

type FilterData struct {
	FilterId *string   `json:"filterId"`
	Call     *callData `json:"call,omitempty"`
	Logs     *callLog  `json:"logs,omitempty"`
}

type callData struct {
	Addr   common.Address `json:"addr"`
	Input  hexutil.Bytes  `json:"input"`
	Output hexutil.Bytes  `json:"output"`
}

type FilterArgs struct {
	FilterId *string         `json:"filterId"`
	ToAddr   *common.Address `json:"toAddr"`
	FromAddr *common.Address `json:"fromAddr"`
	Sels     []FuncSelector  `json:"selectors"`
	Topics   []*common.Hash  `json:"topics"`
}

type callFilter struct {
	filterId string
	toAddr   *common.Address
	fromAddr *common.Address
	sels     map[funcSelector]struct{}
	topics   map[common.Hash]struct{}
}

func (arg *FilterArgs) toCallFilter() *callFilter {
	filter := callFilter{
		filterId: *arg.FilterId,
		toAddr:   arg.ToAddr,
		fromAddr: arg.FromAddr,
		topics:   make(map[common.Hash]struct{}),
		sels:     make(map[funcSelector]struct{}),
	}

	for _, sel := range arg.Sels {
		if len(sel) >= 4 {
			fsel := funcSelector{sel[0], sel[1], sel[2], sel[3]}
			filter.sels[fsel] = struct{}{}
		}
	}
	for _, topic := range arg.Topics {
		filter.topics[*topic] = struct{}{}
	}

	return &filter
}

type TraceFilterLookup struct {
	filters map[string]*callFilter
}

func NewFilterLookup(fargs []FilterArgs) *TraceFilterLookup {
	lookup := TraceFilterLookup{
		filters: make(map[string]*callFilter, 0),
	}

	for _, filter := range fargs {
		if filter.FilterId == nil || (filter.ToAddr == nil && len(filter.Topics) == 0 && len(filter.Sels) == 0) {
			fmt.Println("Invalid filter params")
			return nil
		}
		fmt.Println("add filter:", *filter.FilterId)
		lookup.filters[*filter.FilterId] = filter.toCallFilter()
	}
	return &lookup
}

func (lookup *TraceFilterLookup) filterCallFrame(call *callFrame) []*FilterData {

	results := make([]*FilterData, 0)

	if len(call.Input) < 4 || call.To == nil {
		return results
	}

	for _, filter := range lookup.filters {
		filterData := FilterData{}
		if filter.toAddr != nil && (*filter.toAddr) != *call.To {
			continue
		}

		if filter.fromAddr != nil && (*filter.fromAddr) != call.From {
			continue
		}

		if len(filter.sels) > 0 {
			fsel := funcSelector{call.Input[0], call.Input[1], call.Input[2], call.Input[3]}
			if _, ok := filter.sels[fsel]; !ok {
				continue
			}
		}

		if filter.toAddr != nil || filter.fromAddr != nil || len(filter.sels) > 0 {
			filterData.Call = &callData{*call.To, call.Input, call.Output}
		}

		if len(filter.topics) > 0 {
			for _, calllog := range call.Logs {
				if len(calllog.Topics) == 0 {
					continue
				}
				if _, ok := filter.topics[calllog.Topics[0]]; ok {
					filterData.Logs = &calllog
				}
			}
		}

		if filterData.Call != nil || filterData.Logs != nil {
			filterData.FilterId = &filter.filterId
			results = append(results, &filterData)
		}
	}

	for _, subcall := range call.Calls {
		results = append(results, lookup.filterCallFrame(&subcall)...)
	}

	return results
}

func TracerFindLookupResult(tracer interface{}, lookup *TraceFilterLookup) []*FilterData {
	cTracer, ok := tracer.(*callTracer)
	if !ok {
		fmt.Println("[MEV] recevied no callTracer interface. TypeOf:", reflect.TypeOf(tracer))
		return []*FilterData{}
	}

	return lookup.filterCallFrame(&cTracer.callstack[0])
}
