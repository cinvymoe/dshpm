package remote

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a parsed semantic version.
type Version struct {
	Major int
	Minor int
	Patch int
	Pre   []string
	Raw   string
}

// ParseVersion parses common semantic versions, tolerating a leading v and
// build metadata.
func ParseVersion(raw string) (Version, error) {
	value := strings.TrimSpace(strings.TrimPrefix(raw, "v"))
	value = strings.SplitN(value, "+", 2)[0]
	core := value
	pre := []string(nil)
	if index := strings.Index(value, "-"); index >= 0 {
		core = value[:index]
		pre = strings.Split(value[index+1:], ".")
	}
	parts := strings.Split(core, ".")
	if len(parts) < 1 || len(parts) > 3 {
		return Version{}, fmt.Errorf("invalid version %q", raw)
	}
	numbers := []int{0, 0, 0}
	for i, part := range parts {
		number, err := strconv.Atoi(part)
		if err != nil {
			return Version{}, fmt.Errorf("invalid version %q", raw)
		}
		numbers[i] = number
	}
	return Version{Major: numbers[0], Minor: numbers[1], Patch: numbers[2], Pre: pre, Raw: raw}, nil
}

// CompareVersions returns -1, 0, or 1.
func CompareVersions(a, b string) int {
	left, errLeft := ParseVersion(a)
	right, errRight := ParseVersion(b)
	if errLeft != nil || errRight != nil {
		return strings.Compare(a, b)
	}
	if left.Major != right.Major {
		return compareInt(left.Major, right.Major)
	}
	if left.Minor != right.Minor {
		return compareInt(left.Minor, right.Minor)
	}
	if left.Patch != right.Patch {
		return compareInt(left.Patch, right.Patch)
	}
	if len(left.Pre) == 0 && len(right.Pre) == 0 {
		return 0
	}
	if len(left.Pre) == 0 {
		return 1
	}
	if len(right.Pre) == 0 {
		return -1
	}
	for i := 0; i < len(left.Pre) && i < len(right.Pre); i++ {
		if cmp := comparePre(left.Pre[i], right.Pre[i]); cmp != 0 {
			return cmp
		}
	}
	return compareInt(len(left.Pre), len(right.Pre))
}

func comparePre(a, b string) int {
	ai, aerr := strconv.Atoi(a)
	bi, berr := strconv.Atoi(b)
	switch {
	case aerr == nil && berr == nil:
		return compareInt(ai, bi)
	case aerr == nil:
		return -1
	case berr == nil:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

func compareInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
