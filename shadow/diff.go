package shadow

import (
	"bytes"
	"encoding/json"
	"errors"
	"math/big"
	"reflect"
	"sort"
	"strings"
	"time"
)

// DiffEntry 描述期望配置与上报配置之间的一处差异。
type DiffEntry struct {
	// Path 是差异位置的 JSON Pointer（RFC 6901），如 "/wifi/ssid"。
	Path string
	// DesiredExists 表示期望配置在该路径是否存在值。
	DesiredExists bool
	// ReportedExists 表示上报配置在该路径是否存在值。
	ReportedExists bool
	// Desired 是期望配置在该路径的原始 JSON，不存在时为 nil。
	Desired json.RawMessage
	// Reported 是上报配置在该路径的原始 JSON，不存在时为 nil。
	Reported json.RawMessage
	// Since 是该路径的差异首次出现的时间。
	Since time.Time
}

// computeDiff 计算两份 JSON 对象配置之间的差异，按路径字典序返回。
// desired 与 reported 必须都是 JSON 对象（调用方保证）。
func computeDiff(desired, reported json.RawMessage) []DiffEntry {
	dObj, _ := decodeObject(desired)
	rObj, _ := decodeObject(reported)
	var out []DiffEntry
	walkObject("", dObj, rObj, &out)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// walkObject 递归比较两个对象。prefix 是当前路径前缀。
func walkObject(prefix string, d, r map[string]json.RawMessage, out *[]DiffEntry) {
	keys := make([]string, 0, len(d)+len(r))
	seen := make(map[string]bool, len(d)+len(r))
	for k := range d {
		seen[k] = true
		keys = append(keys, k)
	}
	for k := range r {
		if !seen[k] {
			keys = append(keys, k)
		}
	}
	for _, k := range keys {
		path := prefix + "/" + escapePointer(k)
		dv, dok := d[k]
		rv, rok := r[k]
		switch {
		case dok && !rok:
			// 一侧整个字段不存在：只在该字段处列一条差异，不再下钻。
			*out = append(*out, DiffEntry{Path: path, DesiredExists: true, Desired: cloneRaw(dv)})
		case !dok && rok:
			*out = append(*out, DiffEntry{Path: path, ReportedExists: true, Reported: cloneRaw(rv)})
		default:
			dSub, dIsObj := asObject(dv)
			rSub, rIsObj := asObject(rv)
			if dIsObj && rIsObj {
				// 双方均为对象：逐层比较。
				walkObject(path, dSub, rSub, out)
			} else if !rawEqual(dv, rv) {
				// 其他值（含数组、null）按整体比较，数组不拆分。
				*out = append(*out, DiffEntry{
					Path:           path,
					DesiredExists:  true,
					ReportedExists: true,
					Desired:        cloneRaw(dv),
					Reported:       cloneRaw(rv),
				})
			}
		}
	}
}

// escapePointer 按 RFC 6901 转义路径段。
func escapePointer(s string) string {
	s = strings.ReplaceAll(s, "~", "~0")
	s = strings.ReplaceAll(s, "/", "~1")
	return s
}

// decodeObject 把 raw 解析为 JSON 对象；raw 非对象时返回 false。
// 对象前后只允许 JSON 标准空白（空格、制表符、换行、回车）；
// 对象结束后的第二个 JSON 值或其他非空白文本都视为非法。
func decodeObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, false
	}
	var obj map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&obj); err != nil {
		return nil, false
	}
	if dec.More() { // 对象之后还有非空白内容（第二个值或其他文本）
		return nil, false
	}
	if obj == nil { // "null" 已通过首字符排除，这里防御性处理
		obj = map[string]json.RawMessage{}
	}
	return obj, true
}

func asObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	return decodeObject(raw)
}

// validateConfig 校验配置必须是合法的 JSON 对象，并返回独立副本。
func validateConfig(raw json.RawMessage) (json.RawMessage, error) {
	if _, ok := decodeObject(raw); !ok {
		return nil, ErrInvalidConfig
	}
	return cloneRaw(raw), nil
}

// rawEqual 比较两份 JSON 值是否语义相等：忽略对象字段顺序，
// 数字按数值比较（1 与 1.0 相等），其余按类型与值比较。
func rawEqual(a, b json.RawMessage) bool {
	av, err := decodeValue(a)
	if err != nil {
		return false
	}
	bv, err := decodeValue(b)
	if err != nil {
		return false
	}
	return jsonDeepEqual(av, bv)
}

// errTrailingData 表示 JSON 值之后还有非空白内容，用于内部比较。
var errTrailingData = errors.New("shadow: trailing data after JSON value")

func decodeValue(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() { // 值之后还有非空白内容：不参与相等判断
		return nil, errTrailingData
	}
	return v, nil
}

func jsonDeepEqual(a, b any) bool {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, va := range av {
			vb, ok := bv[k]
			if !ok || !jsonDeepEqual(va, vb) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !jsonDeepEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	case json.Number:
		bv, ok := b.(json.Number)
		if !ok {
			return false
		}
		return numberEqual(av, bv)
	default:
		return reflect.DeepEqual(a, b)
	}
}

func numberEqual(a, b json.Number) bool {
	if a.String() == b.String() {
		return true
	}
	ra, okA := new(big.Rat).SetString(a.String())
	rb, okB := new(big.Rat).SetString(b.String())
	if okA && okB {
		return ra.Cmp(rb) == 0
	}
	return false
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	out := make(json.RawMessage, len(raw))
	copy(out, raw)
	return out
}
