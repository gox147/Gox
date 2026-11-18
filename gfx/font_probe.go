package gfx

// 极小的 sfnt 读取器: 只读表目录 + name 表 + post 表头 (T3-5 第十一批)。
//
// 为什么需要它: 判面 (这个文件里最好的面是正体还是粗体/斜体) 与族索引登记
// 只需要 name 表的三个名字 (family / typographic family / subfamily) 和 post
// 表头的两个字段 (italicAngle / isFixedPitch)。但 x/image 的 col.Font(i) 会把
// maxp/cmap/hhea/hmtx/post 全部解析完 —— CJK 集合字体 10 个面, 每个面连
// cmap、loca 都要建表, 实测 **6~7ms**, 而这 6~7ms 里没有一个字节参与判面。
//
// 这里只做三件事: 按 TTC/单面表头找到每个面的表目录, 读 name 表, 读 post 头。
// 每个面读盘 ≈ 8KB (name 表 3~8KB, 其余都是表头), 10 个面也就 80KB ——
// 比整份解析少 3 个数量级。
//
// **口径必须与 x/image 一致** (判面错档 = 默认字体选成粗体, 整屏变粗且
// fontWeight 看起来失效), 所以下面每个取舍都注明对应的 x/image 行为, 并由
// TestProbeNamesMatchFullParse / TestProbeFileFaceRankMatchesFullParse 用
// 整机字体做等价性兜底。

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"
)

// sfntNameTableCap 是 name 表的读盘上限。真实 name 表 3~8KB (CJK 全集也不到
// 20KB), 1MB 纯粹是"损坏文件别把内存撑爆"的护栏。
const sfntNameTableCap = 1 << 20

// probeFaceInfo 是一个面的判面所需信息 (全部来自 name 表 + post 表头)。
type probeFaceInfo struct {
	family      string // name ID 1, 已 TrimSpace
	typoFamily  string // name ID 16 (没有则空)
	subfamily   string // name ID 2, 已 TrimSpace
	italicAngle float64
	fixedPitch  bool
}

// probeFontFaces 读取一个字体文件里**所有可解析面**的信息。
//
// 返回 nil 表示文件打不开、不是可识别的 sfnt/TTC、或一个面都读不出来 —— 与
// parseFontFile 的失败口径一致。坏的单个面 (缺 name/post 表) 会被跳过, 与
// col.Font(i) 返回 error 时调用方 continue 的行为一致。
func probeFontFaces(path string) ([]probeFaceInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := &countingReaderAt{r: f}

	offsets, err := probeFaceOffsets(r)
	if err != nil {
		return nil, err
	}
	out := make([]probeFaceInfo, 0, len(offsets))
	for _, off := range offsets {
		info, ok := probeFaceAt(r, off)
		if !ok {
			continue
		}
		out = append(out, info)
	}
	fireFontProbeBytesHook(path, r.n)
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no readable face", path)
	}
	return out, nil
}

// countingReaderAt 记录探针一共向文件要了多少字节。
//
// 这是"探针退化成整份解析"唯一可断言的事实: 读盘字节数是确定的, 而毫秒
// 在 CI 上不是 (页缓存会让整份读也显得便宜)。钩子在生产路径上是空操作。
type countingReaderAt struct {
	r io.ReaderAt
	n int
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	c.n += n
	return n, err
}

// probeFaceOffsets 返回文件里每个面的 sfnt 表目录偏移。
//
// TTC 头: tag "ttcf" + version + numFonts + offsets[numFonts]; 单面文件就是
// 偏移 0。dfont (Mac 资源叉) 不支持 —— x/image 支持它, 但 Linux/Windows 上
// 不存在, 真遇到时探针会返回 false, 走整份解析的兜底路径。
func probeFaceOffsets(f io.ReaderAt) ([]uint32, error) {
	var head [12]byte
	if _, err := f.ReadAt(head[:], 0); err != nil {
		return nil, err
	}
	if string(head[0:4]) == "ttcf" {
		n := binary.BigEndian.Uint32(head[8:])
		if n == 0 || n > 1024 {
			return nil, fmt.Errorf("ttc: bad font count %d", n)
		}
		buf := make([]byte, 4*n)
		if _, err := f.ReadAt(buf, 12); err != nil {
			return nil, err
		}
		offsets := make([]uint32, n)
		for i := range offsets {
			offsets[i] = binary.BigEndian.Uint32(buf[4*i:])
		}
		return offsets, nil
	}
	if !knownSFNTVersion(head[0:4]) {
		return nil, fmt.Errorf("not an sfnt file")
	}
	return []uint32{0}, nil
}

// knownSFNTVersion 与 x/image 接受的 sfnt 版本一致。
const (
	sfntVersionTrueType = 0x00010000
	sfntVersionCFF      = 0x4F54544F // "OTTO"
	sfntVersionTrue     = 0x74727565 // "true"
	sfntVersionTyp1     = 0x74797031 // "typ1"
)

func knownSFNTVersion(tag []byte) bool {
	v := binary.BigEndian.Uint32(tag)
	return v == sfntVersionTrueType || v == sfntVersionCFF ||
		v == sfntVersionTrue || v == sfntVersionTyp1
}

// probeFaceAt 读出一个面的判面信息。
//
// 返回 ok=false 的条件**必须比 "x/image 会失败" 更宽松**: 探针只能看到表目录,
// 看不到 cmap 里的格式错误, 所以这里只做"必需表齐全吗"这一层粗筛 (x/image 的
// initialize 缺任何一张都会失败)。真正的判据仍由整份解析给出 —— 探针只是
// "要不要为这个候选付一次整份解析"的启发式, 见 chooseBaseFont。
func probeFaceAt(f io.ReaderAt, off uint32) (probeFaceInfo, bool) {
	var hdr [12]byte
	if _, err := f.ReadAt(hdr[:], int64(off)); err != nil {
		return probeFaceInfo{}, false
	}
	if !knownSFNTVersion(hdr[0:4]) {
		return probeFaceInfo{}, false
	}
	numTables := int(binary.BigEndian.Uint16(hdr[4:]))
	if numTables == 0 || numTables > 4096 {
		return probeFaceInfo{}, false
	}
	dir := make([]byte, 16*numTables)
	if _, err := f.ReadAt(dir, int64(off)+12); err != nil {
		return probeFaceInfo{}, false
	}
	var nameOff, nameLen, postOff, postLen, maxpOff, maxpLen uint32
	have := map[string]bool{}
	for i := 0; i < numTables; i++ {
		e := dir[16*i : 16*i+16]
		tag := string(e[0:4])
		o := binary.BigEndian.Uint32(e[8:])
		l := binary.BigEndian.Uint32(e[12:])
		if l > 0 {
			have[tag] = true
		}
		switch tag {
		case "name":
			nameOff, nameLen = o, l
		case "post":
			postOff, postLen = o, l
		case "maxp":
			maxpOff, maxpLen = o, l
		}
	}
	// x/image 的必需表 (缺一张 initialize 就失败)。字形数据按版本走: TrueType
	// 要 loca+glyf, CFF 要 CFF/CFF2。少了这些的面在整份解析里等于不存在,
	// 探针若把它算进"最好的面", chooseBaseFont 就会先乐观、再被整份解析否掉。
	if !hasRequiredTables(have, hdr[0:4]) {
		return probeFaceInfo{}, false
	}

	info := probeFaceInfo{}
	// name 表解析失败**不是**整面失败: x/image 的 Font() 不碰 name 表, 失败只
	// 发生在 f.Name(), 调用方拿到空串继续判面 (空子族名按"正体"算)。
	nt, nerr := openNameTable(f, nameOff, nameLen)
	if nerr == nil {
		if fam, err := nt.lookup(1); err == nil {
			info.family = fam
		}
		if fam, err := nt.lookup(16); err == nil {
			info.typoFamily = fam
		}
		if sub, err := nt.lookup(2); err == nil {
			info.subfamily = sub
		}
	}

	angle, fixed, ok := probePostHeader(f, postOff, postLen, maxpOff, maxpLen)
	if !ok {
		// post 表坏 ⇒ x/image 的 parsePost 会返回 error ⇒ 整个面不可用。
		return probeFaceInfo{}, false
	}
	info.italicAngle = angle
	info.fixedPitch = fixed
	return info, true
}

// hasRequiredTables 检查 x/image initialize 需要的表是否齐全。
//
// 字形来源三种, 与 sfnt.parseGlyphData 的分支一一对应:
//   - CFF/CFF2 (OTTO)
//   - loca + glyf (普通 TrueType)
//   - CBLC (彩色位图字体: NotoColorEmoji 这类只有 CBDT/CBLC, glyf 是空的,
//     x/image 走"位图分支"、字形集合为空但整份解析**成功**)
//
// name 表**不在**必需之列: x/image 的 Font() 根本不碰它, 缺了照样能用
// (f.Name 报错 ⇒ 子族名空 ⇒ 按正体算)。把它列进来会凭空拒绝可用字体。
func hasRequiredTables(have map[string]bool, version []byte) bool {
	for _, t := range []string{"head", "maxp", "cmap", "hhea", "hmtx", "post"} {
		if !have[t] {
			return false
		}
	}
	if binary.BigEndian.Uint32(version) == sfntVersionCFF {
		return have["CFF "] || have["CFF2"]
	}
	return have["loca"] && have["glyf"] || have["CBLC"]
}

// probeNameTable 是一个已读入记录区的 name 表。
//
// 分两次读 (6 字节表头 + count×12 字节记录) 而不是"整表一次读完": name 表的
// length 字段在真实字体里并不总是可信 (截断、尾部填充都见过), 整表 ReadAt 会
// 因为越过 EOF 直接失败, 而 x/image 只按需读表头/记录/字符串三段, 根本不碰
// 中间那些字节。对齐它的读法才谈得上"同口径"。
type probeNameTable struct {
	f      io.ReaderAt
	off    uint32
	strOff int
	recs   []byte
	count  int
}

func openNameTable(f io.ReaderAt, off, length uint32) (*probeNameTable, error) {
	if length < 6 {
		return nil, fmt.Errorf("name table too short")
	}
	var hdr [6]byte
	if _, err := f.ReadAt(hdr[:], int64(off)); err != nil {
		return nil, err
	}
	count := int(binary.BigEndian.Uint16(hdr[2:]))
	if count > 4096 {
		return nil, fmt.Errorf("name table: %d records", count)
	}
	recs := make([]byte, 12*count)
	// 记录区被截断时只按读到的完整记录算数 (x/image 逐条读, 读到哪算到哪)。
	n, err := f.ReadAt(recs, int64(off)+6)
	if n < len(recs) && err != nil && err != io.EOF {
		return nil, err
	}
	nrec := n / 12
	return &probeNameTable{
		f: f, off: off, strOff: int(binary.BigEndian.Uint16(hdr[4:])),
		recs: recs[:nrec*12], count: nrec,
	}, nil
}

// lookup 取第一个"可解码"的指定 ID 记录 (与 x/image 的 Font.Name 同序:
// 只认 (1,0) MacRoman 与 (3,1) UCS-2, 其它平台跳过而不是报错)。
func (t *probeNameTable) lookup(id uint16) (string, error) {
	for i := 0; i < t.count; i++ {
		e := t.recs[12*i : 12*i+12]
		if binary.BigEndian.Uint16(e[6:]) != id {
			continue
		}
		pid := binary.BigEndian.Uint16(e[0:])
		eid := binary.BigEndian.Uint16(e[2:])
		var ucs2 bool
		switch {
		case pid == 3 && eid == 1:
			ucs2 = true
		case pid == 1 && eid == 0:
			ucs2 = false
		default:
			continue
		}
		l := int(binary.BigEndian.Uint16(e[8:]))
		start := t.strOff + int(binary.BigEndian.Uint16(e[10:]))
		buf := make([]byte, l)
		if n, err := t.f.ReadAt(buf, int64(t.off)+int64(start)); n < l {
			if err == nil || err == io.EOF {
				return "", fmt.Errorf("name record truncated")
			}
			return "", err
		}
		return decodeNameBytes(buf, ucs2)
	}
	return "", fmt.Errorf("name id %d not found", id)
}

// decodeNameBytes 解码一条 name 记录。
//
// UCS-2 逐 2 字节取一个 rune, 奇数长度报错 —— 与 x/image 的 stringifyUCS2
// 完全一致 (它也不处理代理对)。MacRoman 只处理 ASCII: 判面用的关键词
// (Bold/Italic/Regular/Negreta/fett…) 全是 ASCII, 高位字节替换成 U+FFFD 不会
// 制造也不会破坏任何一次子串命中, 因此不必为它引入 x/text/charmap。
func decodeNameBytes(b []byte, ucs2 bool) (string, error) {
	if ucs2 {
		if len(b)&1 != 0 {
			return "", fmt.Errorf("odd UCS-2 length")
		}
		r := make([]rune, len(b)/2)
		for i := range r {
			r[i] = rune(binary.BigEndian.Uint16(b[2*i:]))
		}
		return strings.TrimSpace(string(r)), nil
	}
	r := make([]rune, len(b))
	for i, c := range b {
		if c < 0x80 {
			r[i] = rune(c)
		} else {
			r[i] = 0xFFFD
		}
	}
	return strings.TrimSpace(string(r)), nil
}

// probePostHeader 读 post 表头的 italicAngle 与 isFixedPitch。
//
// 与 x/image 的 parsePost 对齐: 版本只认 0x10000 / 0x20000 / 0x30000, 表长
// 至少 32 字节; 版本 2.0 还要 2 + 2*numGlyphs 字节 (numGlyphs 来自 maxp)。
// 任一不满足 ⇒ ok=false (整面不可用), 因为 post 是 x/image 的必需表。
func probePostHeader(f io.ReaderAt, off, length, maxpOff, maxpLen uint32) (angle float64, fixed bool, ok bool) {
	const headerSize = 32
	if length < headerSize {
		return 0, false, false
	}
	var buf [headerSize]byte
	if _, err := f.ReadAt(buf[:], int64(off)); err != nil {
		return 0, false, false
	}
	switch binary.BigEndian.Uint32(buf[0:]) {
	case 0x10000, 0x30000:
	case 0x20000:
		// 版本 2.0 后面跟 glyphNameIndex[] —— 截断的表 x/image 会拒绝。
		if maxpLen < 6 {
			return 0, false, false
		}
		var m [6]byte
		if _, err := f.ReadAt(m[:], int64(maxpOff)); err != nil {
			return 0, false, false
		}
		numGlyphs := uint32(binary.BigEndian.Uint16(m[4:]))
		if length < headerSize+2+2*numGlyphs {
			return 0, false, false
		}
	default:
		return 0, false, false
	}
	angle = float64(int32(binary.BigEndian.Uint32(buf[4:]))) / 0x10000
	return angle, binary.BigEndian.Uint32(buf[12:]) != 0, true
}

// fontProbeBytesHook 是"探针这次一共读了多少字节"的测试钩子 (仅测试期非 nil)。
//
// 与 fontProbeHook (打开过哪个文件) 分开: 前者守"试过哪个候选", 这条守
// "试它的代价"。T3-5 第十一批把判面从 col.Font(i) 换成手读 name/post 表头,
// 一旦有人在探针里加回整份解析, 字节数会从 ~80KB 跳到 19MB —— 毫秒断言在
// CI 上看不出来 (页缓存), 字节数看得出来。
var fontProbeBytesHook func(path string, n int)

func fireFontProbeBytesHook(path string, n int) {
	if h := fontProbeBytesHook; h != nil {
		h(path, n)
	}
}
