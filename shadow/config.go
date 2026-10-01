package shadow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
)

// validateObjectConfig 校验 raw 是一个 JSON 对象，且不含重复键与尾随数据。
func validateObjectConfig(raw json.RawMessage) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Errorf("%w: 内容为空", ErrInvalidConfig)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	var walk func() error
	walk = func() error {
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidConfig, err)
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{':
				seen := map[string]struct{}{}
				for dec.More() {
					kt, err := dec.Token()
					if err != nil {
						return fmt.Errorf("%w: %v", ErrInvalidConfig, err)
					}
					key, ok := kt.(string)
					if !ok {
						return fmt.Errorf("%w: 键不是字符串", ErrInvalidConfig)
					}
					if _, dup := seen[key]; dup {
						return fmt.Errorf("%w: 重复键 %q", ErrInvalidConfig, key)
					}
					seen[key] = struct{}{}
					if err := walk(); err != nil {
						return err
					}
				}
				if _, err := dec.Token(); err != nil {
					return fmt.Errorf("%w: %v", ErrInvalidConfig, err)
				}
			case '[':
				for dec.More() {
					if err := walk(); err != nil {
						return err
					}
				}
				if _, err := dec.Token(); err != nil {
					return fmt.Errorf("%w: %v", ErrInvalidConfig, err)
				}
			default:
				return fmt.Errorf("%w: 意外的分隔符 %q", ErrInvalidConfig, d)
			}
		}
		return nil
	}
	if err := walk(); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("%w: 存在尾随数据", ErrInvalidConfig)
	}
	if bytes.TrimSpace(raw)[0] != '{' {
		return fmt.Errorf("%w: 顶层必须是 JSON 对象", ErrInvalidConfig)
	}
	return nil
}

// parseConfig 把配置解析为可比较的 Go 值。
func parseConfig(raw json.RawMessage) (interface{}, error) {
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// configsEqual 按 JSON 语义比较两份配置是否相等。
func configsEqual(a, b json.RawMessage) bool {
	va, err := parseConfig(a)
	if err != nil {
		return false
	}
	vb, err := parseConfig(b)
	if err != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}

// diffEntry 是一条内部差异记录。
type diffEntry struct {
	path    string
	dExists bool
	dValue  interface{}
	rExists bool
	rValue  interface{}
}

// collectDiffs 递归收集差异。
// 双方均为对象时按键逐层比较；其他值整体比较，数组不拆分；
// 任一侧字段不存在时只在该路径列一条差异。
func collectDiffs(path string, d, r interface{}, dExists, rExists bool, out *[]diffEntry) {
	switch {
	case dExists && rExists:
		dm, dIsObj := d.(map[string]interface{})
		rm, rIsObj := r.(map[string]interface{})
		if dIsObj && rIsObj {
			keys := make(map[string]struct{})
			for k := range dm {
				keys[k] = struct{}{}
			}
			for k := range rm {
				keys[k] = struct{}{}
			}
			sorted := make([]string, 0, len(keys))
			for k := range keys {
				sorted = append(sorted, k)
			}
			sort.Strings(sorted)
			for _, k := range sorted {
				dv, dok := dm[k]
				rv, rok := rm[k]
				collectDiffs(path+"/"+jsonPointerEscape(k), dv, rv, dok, rok, out)
			}
			return
		}
		if !reflect.DeepEqual(d, r) {
			*out = append(*out, diffEntry{
				path:    path,
				dExists: true,
				dValue:  d,
				rExists: true,
				rValue:  r,
			})
		}
	case dExists:
		*out = append(*out, diffEntry{
			path:    path,
			dExists: true,
			dValue:  d,
		})
	case rExists:
		*out = append(*out, diffEntry{
			path:    path,
			rExists: true,
			rValue:  r,
		})
	}
}

// computeDiffEntries 计算两份配置的全部差异。
func computeDiffEntries(desired, reported json.RawMessage) ([]diffEntry, error) {
	d, err := parseConfig(desired)
	if err != nil {
		return nil, err
	}
	r, err := parseConfig(reported)
	if err != nil {
		return nil, err
	}
	var out []diffEntry
	collectDiffs("", d, r, true, true, &out)
	return out, nil
}

// refreshDiffs 刷新差异计时：
// 仍存在的差异保留原首次出现时间，新差异使用 now，已消失的差异移除。
func refreshDiffs(prev map[string]time.Time, entries []diffEntry, now time.Time) map[string]time.Time {
	out := make(map[string]time.Time, len(entries))
	for _, e := range entries {
		if t, ok := prev[e.path]; ok {
			out[e.path] = t
		} else {
			out[e.path] = now
		}
	}
	return out
}

// jsonPointerEscape 按 RFC 6901 转义 JSON Pointer 片段。
func jsonPointerEscape(key string) string {
	key = strings.ReplaceAll(key, "~", "~0")
	key = strings.ReplaceAll(key, "/", "~1")
	return key
}

// cloneRaw 深拷贝一段 JSON 原始字节。
func cloneRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	out := make(json.RawMessage, len(raw))
	copy(out, raw)
	return out
}

// marshalValue 把解析后的 Go 值还原为 JSON 原始字节。
func marshalValue(v interface{}) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}
