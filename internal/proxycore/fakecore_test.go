package proxycore

// 假内核：测试二进制在环境变量 PROXYCORE_FAKE_CORE=1 时扮演 mihomo，实现网关用到的那几条命令行与控制接口：
//
//	-v                    打印版本
//	-t -d DIR -f FILE     校验配置（配置里出现 FAIL_VALIDATION 就拒绝）
//	-d DIR -f FILE        运行：/version、PUT /configs、/providers/proxies/{name}、…/healthcheck
//
// 运行时每次（重新）加载配置都往 DIR/loads 追加一行；DIR/crash-once 存在时，启动后 300ms 自己退出一次。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("PROXYCORE_FAKE_CORE") == "1" {
		os.Exit(fakeCore(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func argAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func fakeCore(args []string) int {
	if len(args) > 0 && args[0] == "-v" {
		fmt.Println("Mihomo Meta v9.9.9 fake/test with go")
		return 0
	}
	dir, file := argAfter(args, "-d"), argAfter(args, "-f")
	raw, err := os.ReadFile(file)
	if err != nil {
		fmt.Println("read config:", err)
		return 1
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		fmt.Println("parse config:", err)
		return 1
	}
	if len(args) > 0 && args[0] == "-t" {
		if strings.Contains(string(raw), "FAIL_VALIDATION") {
			fmt.Println("level=error msg=\"proxy group[0]: bad filter\"")
			return 1
		}
		fmt.Printf("configuration file %s test is successful\n", file)
		return 0
	}

	var mu sync.Mutex
	current := cfg
	record := func() {
		f, _ := os.OpenFile(filepath.Join(dir, "loads"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		fmt.Fprintln(f, time.Now().UnixNano())
		f.Close()
	}
	record()
	secret, _ := cfg["secret"].(string)
	nameRe := regexp.MustCompile(`"?name"?:\s*"?([^",}\n]+)`)
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+secret {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("/version", auth(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"meta":true,"version":"v9.9.9"}`)
	}))
	mux.HandleFunc("/configs", auth(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Path string `json:"path"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		b, err := os.ReadFile(body.Path)
		var next map[string]any
		if err == nil {
			err = json.Unmarshal(b, &next)
		}
		if err != nil || !filepath.IsAbs(body.Path) {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"message":"bad path %v"}`, err)
			return
		}
		mu.Lock()
		current = next
		mu.Unlock()
		record()
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("/providers/proxies/", auth(func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/providers/proxies/")
		if strings.HasSuffix(rest, "/healthcheck") {
			fmt.Fprint(w, `{"delay":42}`)
			return
		}
		mu.Lock()
		provs, _ := current["proxy-providers"].(map[string]any)
		mu.Unlock()
		p, ok := provs[rest].(map[string]any)
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"resource not found"}`)
			return
		}
		content, _ := os.ReadFile(filepath.Join(dir, p["path"].(string)))
		text := string(content)
		if i := strings.Index(text, "\nproxy-groups:"); i >= 0 {
			text = text[:i]
		}
		var out []map[string]any
		for _, m := range nameRe.FindAllStringSubmatch(text, -1) {
			out = append(out, map[string]any{"name": strings.TrimSpace(m[1]), "type": "Trojan", "history": []any{}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"name": rest, "proxies": out})
	}))
	if _, err := os.Stat(filepath.Join(dir, "crash-once")); err == nil {
		_ = os.Remove(filepath.Join(dir, "crash-once"))
		go func() {
			time.Sleep(300 * time.Millisecond)
			fmt.Println("level=fatal msg=\"simulated crash\"")
			os.Exit(3)
		}()
	}
	addr, _ := cfg["external-controller"].(string)
	fmt.Println("level=info msg=\"RESTful API listening at:", addr+"\"")
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Println("listen:", err)
		return 1
	}
	return 0
}
