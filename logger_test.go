package logger_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"

	logger "github.com/nickham-su/go_logger"
)

func shanghaiDate() string {
	loc := time.FixedZone("Asia/Shanghai", 8*3600)
	return time.Now().In(loc).Format("2006-01-02")
}

func TestLoggerBasicBehavior(t *testing.T) {
	// 该测试依赖“全局单例 + 配置只生效一次”的语义，因此集中在一个用例里顺序验证。

	var buf1 bytes.Buffer
	var buf2 bytes.Buffer
	var buf3 bytes.Buffer

	dir1 := t.TempDir()
	dir2 := t.TempDir()

	// 验证 SetDir/AppendWriter 仅第一次调用生效（在首次写日志前）。
	logger.SetDir(dir1)
	logger.SetDir(dir2) // 应被忽略

	logger.AppendWriter(&buf1)
	logger.AppendWriter(&buf2) // 应被忽略

	logger.Info.Println("hello")

	// 1) 输出格式：时间戳 + LEVEL + 内容
	line := buf1.String()
	re := regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d{6} INFO hello\n$`)
	if !re.MatchString(line) {
		t.Fatalf("输出格式不符合预期：%q", line)
	}

	// 2) 第一次 AppendWriter 生效，后续忽略
	if buf2.Len() != 0 {
		t.Fatalf("AppendWriter 第二次调用应被忽略，但 buf2 非空：%q", buf2.String())
	}

	// 3) 第一次 SetDir 生效，后续忽略（日志文件应写入 dir1）
	date := shanghaiDate()
	infoFile1 := filepath.Join(dir1, date+".info.log")
	if _, err := os.Stat(infoFile1); err != nil {
		t.Fatalf("期望日志文件存在于首次 SetDir 的目录：%s, err=%v", infoFile1, err)
	}
	infoFile2 := filepath.Join(dir2, date+".info.log")
	if _, err := os.Stat(infoFile2); err == nil {
		t.Fatalf("不应在第二次 SetDir 的目录创建日志文件：%s", infoFile2)
	}

	// 4) Printf 行为：应包含级别前缀，且最终仅一行（末尾换行由 log 包保证）
	logger.Warning.Printf("x=%d", 1)
	lines := bytes.Split([]byte(buf1.String()), []byte("\n"))
	// buf1 已经至少有两行（hello 与 x=1），Split 会在末尾多一个空切片
	if len(lines) < 3 {
		t.Fatalf("期望至少两行输出，实际：%q", buf1.String())
	}
	lastNonEmpty := string(lines[len(lines)-2]) // 倒数第二个是最后一行内容
	re2 := regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d{6} WARNING x=1$`)
	if !re2.MatchString(lastNonEmpty) {
		t.Fatalf("Printf 输出不符合预期：%q", lastNonEmpty)
	}

	// 5) 首次写入后配置冻结：AppendWriter 再次调用应被忽略
	logger.AppendWriter(&buf3)
	logger.Info.Println("after")
	if buf3.Len() != 0 {
		t.Fatalf("首次写入后 AppendWriter 应被忽略，但 buf3 非空：%q", buf3.String())
	}

	// 6) 并发写入：不要求断言内容，只要在 -race 下无数据竞争且不 panic
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				logger.Info.Printf("g=%d j=%d", id, j)
			}
		}(i)
	}
	wg.Wait()
}

func TestSetTimezoneInvalidExits(t *testing.T) {
	if os.Getenv("LOGGER_TEST_TZFAIL") == "1" {
		// 该调用会触发 log.Fatalln -> os.Exit(1)
		logger.SetTimezone("Invalid/Zone")
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run", "^TestSetTimezoneInvalidExits$")
	cmd.Env = append(os.Environ(), "LOGGER_TEST_TZFAIL=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("期望子进程退出失败，但返回成功：%s", string(out))
	}
	if !bytes.Contains(out, []byte("加载时区失败")) {
		t.Fatalf("期望错误输出包含“加载时区失败”，实际：%s", string(out))
	}
}

func TestSetDirFailureFallsBack(t *testing.T) {
	// 由于 SetDir 仅第一次调用生效，这个用例用子进程隔离全局状态。
	if os.Getenv("LOGGER_TEST_DIRFAIL") == "1" {
		// 制造一个稳定失败场景：先创建同名文件，再尝试在其下创建目录。
		_ = os.WriteFile("block", []byte("x"), 0644)
		logger.SetDir(filepath.Join("block", "subdir"))
		logger.Info.Println("dir-fallback")

		infoFile := shanghaiDate() + ".info.log"
		if _, err := os.Stat(infoFile); err != nil {
			t.Fatalf("期望降级后日志写入当前目录：%s, err=%v", infoFile, err)
		}
		return
	}

	workdir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run", "^TestSetDirFailureFallsBack$")
	cmd.Dir = workdir
	cmd.Env = append(os.Environ(), "LOGGER_TEST_DIRFAIL=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("子进程应成功退出，err=%v, out=%s", err, string(out))
	}
}
