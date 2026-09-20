package mission

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// StableHash 对任意 JSON 可序列化值计算与字段顺序无关的稳定哈希。
// 用于冻结遥测帧与指令版本的指纹。
func StableHash(v any) string {
	h := sha256.New()
	enc := json.NewEncoder(h)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(canonical(v))
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// canonical 将 map 按键排序、切片保序地规范化，保证哈希稳定。
func canonical(v any) any {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([][2]any, 0, len(t))
		for _, k := range keys {
			out = append(out, [2]any{k, canonical(t[k])})
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i := range t {
			out[i] = canonical(t[i])
		}
		return out
	default:
		return v
	}
}

// HashString 返回字符串的短哈希，用于生成确定性标识。
func HashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}
