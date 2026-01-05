package logger

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// logLevel 表示日志级别（越大越严重）。
type logLevel int

const (
	// levelDebug 调试日志。
	levelDebug logLevel = iota
	// levelInfo 普通信息日志。
	levelInfo
	// levelWarning 告警日志。
	levelWarning
	// levelError 错误日志。
	levelError
)

var (
	// Debug 默认 Debug 级别日志实例（按天切割：*.debug.log）。
	Debug *logger
	// Info 默认 Info 级别日志实例（按天切割：*.info.log）。
	Info *logger
	// Warning 默认 Warning 级别日志实例（按天切割：*.warning.log）。
	Warning *logger
	// Error 默认 Error 级别日志实例（按天切割：*.error.log；额外提供 Fatal 系列方法）。
	Error *errorLogger

	// rotateMu 串行化全局轮转（跨天切割/配置变更导致的文件名更新），避免并发重复轮转。
	rotateMu sync.Mutex
	// dateStr 是最近一次轮转后的日期（格式：2006-01-02），仅在 rotateMu 保护下读写。
	dateStr string
	// dateFast 是 dateStr 的无锁快照，用于 rotateIfNeeded 快路径判断，减少绝大多数写入的加锁开销。
	dateFast atomic.Value // string

	// configMu 用于“配置冻结”：
	// - SetTimezone/AppendWriter/SetDir 要求在第一次写日志前调用
	// - 首次写入会将 started 置位，并等待正在进行的配置写入完成，彻底消除并发竞态窗口
	configMu sync.Mutex

	// started 标记是否已经开始写日志；一旦为 true，配置类方法将静默忽略。
	started atomic.Bool

	// setTimezoneOnce 确保 SetTimezone 仅第一次调用生效。
	setTimezoneOnce sync.Once
	// appendWriterOnce 确保 AppendWriter 仅第一次调用生效。
	appendWriterOnce sync.Once
	// setDirOnce 确保 SetDir 仅第一次调用生效。
	setDirOnce sync.Once

	// dirPathValue 保存日志目录（string）。配置冻结后不再改变。
	dirPathValue atomic.Value // string
	// locationValue 保存日志时区（*time.Location）。默认上海时区；配置冻结后不再改变。
	locationValue atomic.Value // *time.Location
	// writersValue 保存额外输出目标（[]io.Writer），如 os.Stdout。配置冻结后不再改变。
	writersValue atomic.Value // []io.Writer
)

// init 初始化默认配置与默认 logger。
//
// 约定：
// - 默认时区：上海（Asia/Shanghai，固定 +8，避免容器缺 tzdata）
// - 默认目录：空字符串（表示不额外加目录前缀）
// - 默认额外 writer：无
//
// 注意：此处仅初始化文件名；文件句柄在首次写入时懒打开。
func init() {
	dirPathValue.Store("")
	locationValue.Store(time.FixedZone("Asia/Shanghai", 8*3600))
	writersValue.Store([]io.Writer(nil))
	// 先初始化 dateFast，避免未来改动引入“Load 先于 Store”导致 panic。
	dateFast.Store("")

	rotate(nowInLocation().Format("2006-01-02"))
}

// createLogger 重新计算并设置“当天”的日志文件名（强制执行一次轮转逻辑）。
//
// 注意：不会打开文件句柄，仅更新文件名；文件会在首次写入时懒打开。
func createLogger() {
	rotate(nowInLocation().Format("2006-01-02"))
}

// rotateLocked 在持有 rotateMu 的前提下执行一次轮转：
// - 更新 dateStr/dateFast
// - 生成各级别文件名
// - 对已存在的 logger：关闭旧文件句柄并清空内部 logger，使下一次写入懒初始化新文件
func rotateLocked(date string) {
	dateStr = date
	dateFast.Store(date)
	dirPath := dirPathValue.Load().(string)
	debugFile := filepath.Join(dirPath, date+".debug.log")
	infoFile := filepath.Join(dirPath, date+".info.log")
	warningFile := filepath.Join(dirPath, date+".warning.log")
	errorFile := filepath.Join(dirPath, date+".error.log")

	if Debug == nil {
		Debug = newLogger(levelDebug, debugFile)
	} else {
		Debug.setFileName(debugFile)
	}

	if Info == nil {
		Info = newLogger(levelInfo, infoFile)
	} else {
		Info.setFileName(infoFile)
	}

	if Warning == nil {
		Warning = newLogger(levelWarning, warningFile)
	} else {
		Warning.setFileName(warningFile)
	}

	if Error == nil {
		Error = newErrorLogger(levelError, errorFile)
	} else {
		Error.logger.setFileName(errorFile)
	}
}

// SetTimezone 设置日志时区（仅第一次调用生效，后续调用会被忽略；且需在第一次写日志前调用）。
//
// 推荐传入 "Asia/Shanghai"；该值会走固定时区实现，避免容器缺少 tzdata 导致加载失败。
//
// 该时区会同时影响：
// - 按天切割：使用该时区计算“今天”的日期键
// - 日志时间戳：输出时间统一按该时区格式化
func SetTimezone(name string) {
	if name == "" {
		return
	}
	if started.Load() {
		return
	}

	// 配置冻结互斥：避免与首次写日志并发时出现“started 已置位但配置仍然生效”的竞态。
	configMu.Lock()
	defer configMu.Unlock()
	if started.Load() {
		return
	}

	setTimezoneOnce.Do(func() {
		var loc *time.Location
		if name == "Asia/Shanghai" {
			loc = time.FixedZone("Asia/Shanghai", 8*3600)
		} else {
			loaded, err := time.LoadLocation(name)
			if err != nil {
				log.Fatalln("加载时区失败：", name, err)
			}
			loc = loaded
		}

		locationValue.Store(loc)
		// 时区变化可能导致“当前日期键”不同，因此强制重算文件名。
		rotate(nowInLocation().Format("2006-01-02"))
	})
}

// AppendWriter 追加日志输出目标（仅第一次调用生效，后续调用会被忽略；且需在第一次写日志前调用）。
//
// 例：AppendWriter(os.Stdout)
//
// 注意：本库约定配置只在启动阶段设置；不会在运行中动态追加输出目标。
func AppendWriter(writer ...io.Writer) {
	if len(writer) == 0 {
		return
	}
	if started.Load() {
		return
	}

	// 配置冻结互斥：避免与首次写日志并发时出现“started 已置位但配置仍然生效”的竞态。
	configMu.Lock()
	defer configMu.Unlock()
	if started.Load() {
		return
	}

	appendWriterOnce.Do(func() {
		// 拷贝一份切片，避免外部后续修改底层数组导致不可预期行为。
		cp := make([]io.Writer, len(writer))
		copy(cp, writer)
		writersValue.Store(cp)
	})
}

// SetDir 设置日志目录（仅第一次调用生效；且需在第一次写日志前调用）。
//
// 说明：
// - 使用 filepath.Clean 规整路径
// - 使用 os.MkdirAll 创建多级目录（若失败会降级为当前目录写日志，并打印错误）
func SetDir(path string) {
	if path == "" {
		return
	}
	if started.Load() {
		return
	}

	// 配置冻结互斥：避免与首次写日志并发时出现“started 已置位但配置仍然生效”的竞态。
	configMu.Lock()
	defer configMu.Unlock()
	if started.Load() {
		return
	}

	setDirOnce.Do(func() {
		cleaned := filepath.Clean(path)
		if err := os.MkdirAll(cleaned, os.ModePerm); err != nil {
			log.Println("创建日志目录失败，降级为当前目录写日志：", cleaned, err)
			return
		}

		dirPathValue.Store(cleaned)
		// 目录变化即使不跨天，也必须强制更新文件名。
		createLogger()
	})
}

// nowInLocation 返回当前时间，并转换为当前配置时区。
func nowInLocation() time.Time {
	loc := locationValue.Load().(*time.Location)
	return time.Now().In(loc)
}

// rotate 串行化执行一次轮转（无条件调用 rotateLocked）。
// 用于目录/时区等配置变更，确保文件名被强制刷新。
func rotate(date string) {
	rotateMu.Lock()
	rotateLocked(date)
	rotateMu.Unlock()
}

// rotateIfDateChanged 在持锁后做二次确认：只有日期变化才轮转。
// 用于写入前触发的“跨天切割”，避免跨天瞬间重复轮转。
func rotateIfDateChanged(date string) {
	rotateMu.Lock()
	if dateStr != date {
		rotateLocked(date)
	}
	rotateMu.Unlock()
}

// rotateIfNeeded 写入前触发的按天切割：
// - 先用 dateFast 快速判断（无锁）
// - 若可能跨天，再进入锁内二次确认并轮转
func rotateIfNeeded(now time.Time) {
	date := now.Format("2006-01-02")
	if cur := dateFast.Load().(string); cur == date {
		return
	}
	rotateIfDateChanged(date)
}

// freezeConfig 冻结配置并等待正在进行的配置写入完成。
//
// 目标：
// - 彻底消除“首次写日志 vs SetXXX 并发”的竞态窗口
// - 冻结完成后，绝大多数写入走无锁快路径（不再每次都加 configMu）
func freezeConfig() {
	if started.Load() {
		return
	}

	configMu.Lock()
	started.Store(true)
	configMu.Unlock()
}

// timePrefix 生成日志时间戳前缀（格式：2006-01-02 15:04:05.000000）。
func timePrefix(now time.Time) string {
	return now.Format("2006/01/02 15:04:05.000000")
}

// levelString 将日志级别转换为字符串标识。
func levelString(level logLevel) string {
	switch level {
	case levelDebug:
		return "DEBUG"
	case levelInfo:
		return "INFO"
	case levelWarning:
		return "WARNING"
	case levelError:
		return "ERROR"
	default:
		return "UNKNOWN"
	}
}

// withFileWriter 合并额外输出目标与文件输出目标。
// 输出顺序：先额外 writer（如 Stdout），再写文件。
func withFileWriter(file io.Writer) []io.Writer {
	base := writersValue.Load().([]io.Writer)
	w := make([]io.Writer, 0, len(base)+1)
	w = append(w, base...)
	w = append(w, file)
	return w
}

// newLogger 创建一个普通 logger；文件句柄在首次写入时懒打开。
func newLogger(level logLevel, fileName string) *logger {
	return &logger{
		logger:   nil,
		file:     nil,
		fileName: fileName,
		level:    level,
	}
}

// logger 是基础日志实现：
// - l.mu 保护文件句柄/懒初始化/轮转关闭
// - l.logger 输出会同时写入额外 writer 与文件
type logger struct {
	logger   *log.Logger
	file     *os.File
	fileName string
	level    logLevel
	mu       sync.Mutex
}

// ensureLoggerLocked 在持有 l.mu 的前提下，确保内部 *log.Logger 已初始化。
//
// 约定：
// - log.New flags=0：不使用标准库的默认时间戳/前缀
// - 时间戳与级别前缀由上层 Println/Printf 统一拼接
func (l *logger) ensureLoggerLocked() {
	if l.logger != nil {
		return
	}

	file, err := os.OpenFile(l.fileName, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		log.Fatalln("打开日志文件失败：", err)
	}
	l.file = file
	w := withFileWriter(file)
	l.logger = log.New(io.MultiWriter(w...), "", 0)
}

// setFileName 更新 logger 的文件名：
// - 若文件名发生变化且已打开文件，则关闭旧文件句柄并清空内部 logger
// - 下一次写入会重新懒初始化打开新文件
func (l *logger) setFileName(fileName string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.fileName == fileName {
		return
	}
	if l.file != nil {
		// 关闭旧文件句柄，避免跨天/切目录后句柄泄露。
		_ = l.file.Close()
		l.file = nil
		l.logger = nil
	}
	l.fileName = fileName
}

// Close 主动关闭当前 logger 已打开的文件句柄（若未打开则无操作）。
// 注意：关闭后下一次写入会重新懒初始化打开文件。
func (l *logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	l.logger = nil
	return err
}

// Println 输出一条日志（写入前触发按天切割）。
//
// 首次写入会冻结配置：之后 SetTimezone/AppendWriter/SetDir 均会被忽略（静默返回）。
func (l *logger) Println(v ...interface{}) {
	freezeConfig()

	now := nowInLocation()
	rotateIfNeeded(now)
	ts := timePrefix(now)
	level := levelString(l.level)

	l.mu.Lock()
	defer l.mu.Unlock()

	l.ensureLoggerLocked()
	args := make([]interface{}, 0, len(v)+2)
	args = append(args, ts, level)
	args = append(args, v...)
	l.logger.Println(args...)
}

// Printf 格式化输出一条日志（写入前触发按天切割；不自动补换行，沿用 log.Printf 行为）。
func (l *logger) Printf(format string, v ...interface{}) {
	freezeConfig()

	now := nowInLocation()
	rotateIfNeeded(now)
	ts := timePrefix(now)
	level := levelString(l.level)

	l.mu.Lock()
	defer l.mu.Unlock()

	l.ensureLoggerLocked()
	l.logger.Printf(ts+" "+level+" "+format, v...)
}

// newErrorLogger 创建一个带 Fatal 能力的 errorLogger（内部复用 logger）。
func newErrorLogger(level logLevel, fileName string) *errorLogger {
	return &errorLogger{
		logger{
			logger:   nil,
			file:     nil,
			fileName: fileName,
			level:    level,
		},
	}
}

// errorLogger 是 Error 级别日志封装，额外提供 Fatal* 方法退出进程。
type errorLogger struct {
	logger
}

// Close 关闭文件句柄（同 logger.Close）。
func (l *errorLogger) Close() error {
	return l.logger.Close()
}

// Println 输出一条错误日志（同 logger.Println）。
func (l *errorLogger) Println(v ...interface{}) {
	l.logger.Println(v...)
}

// Printf 格式化输出一条错误日志（同 logger.Printf）。
func (l *errorLogger) Printf(format string, v ...interface{}) {
	l.logger.Printf(format, v...)
}

// Fatalln 输出错误日志并退出进程（exit code=1）。
// 注意：作为库方法，它会直接终止进程；请谨慎在业务中使用。
func (l *errorLogger) Fatalln(v ...interface{}) {
	l.logger.Println(v...)
	os.Exit(1)
}

// Fatalf 格式化输出错误日志并退出进程（exit code=1）。
// 注意：作为库方法，它会直接终止进程；请谨慎在业务中使用。
func (l *errorLogger) Fatalf(format string, v ...interface{}) {
	l.logger.Printf(format, v...)
	os.Exit(1)
}
