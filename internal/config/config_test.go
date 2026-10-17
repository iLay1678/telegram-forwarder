package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func withoutEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, key := range keys {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
}

func writeEnv(t *testing.T, root, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyDotEnvPriorityAndEmptyValues(t *testing.T) {
	withoutEnv(t, "API_ID", "API_HASH", "PHONE_NUMBER", "BOT_TOKEN", "USER_ID", "ADMINS", "DEFAULT_TIMEZONE", "DATABASE_URL", "TF_TEST_QUOTED", "TF_TEST_EMPTY", "TF_TEST_MULTILINE")
	root := t.TempDir()
	writeEnv(t, root, `API_ID=12345
API_HASH=test_hash
PHONE_NUMBER=+8610000000000
BOT_TOKEN=test_token
USER_ID=90001
ADMINS=90001, 90002
DEFAULT_TIMEZONE=Asia/Shanghai
DATABASE_URL=sqlite:///./db/forward.db
TF_TEST_QUOTED="value # inside" # outside
TF_TEST_EMPTY=
TF_TEST_MULTILINE="line1\nline2"
`)
	t.Setenv("API_HASH", "environment_override")
	c, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if c.APIID != 12345 || c.APIHash != "environment_override" || c.UserID != 90001 || !reflect.DeepEqual(c.Admins, []int64{90001, 90002}) {
		t.Fatalf("配置解析错误: %+v", c)
	}
	if c.String("TF_TEST_QUOTED", "") != "value # inside" || c.String("TF_TEST_EMPTY", "fallback") != "" || c.String("TF_TEST_MULTILINE", "") != "line1\nline2" {
		t.Fatal("dotenv 引号、空字符串或转义不兼容")
	}
	if c.DatabasePath != filepath.Join(root, "db", "forward.db") {
		t.Fatal(c.DatabasePath)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestMissingCredentialsDeferredAndDefaultFilesPreserved(t *testing.T) {
	withoutEnv(t, "API_ID", "API_HASH", "PHONE_NUMBER", "BOT_TOKEN", "USER_ID", "ADMINS", "DEFAULT_TIMEZONE", "DATABASE_URL")
	root := t.TempDir()
	c, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if c.Validate() == nil {
		t.Fatal("实际启动必须验证凭据")
	}
	if err := os.WriteFile(c.Path("config/ai_models.json"), []byte(`{"custom":["my-model"]}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(c.Path("config/ai_models.json"))
	if err != nil || string(content) != `{"custom":["my-model"]}` {
		t.Fatal("现有模型配置被覆盖")
	}
	checks := map[string]struct {
		count       int
		first, last string
	}{
		"summary_times.txt": {49, "00:00", "23:50"}, "delay_times.txt": {10, "1", "10"},
		"max_media_size.txt": {47, "1", "2048"}, "media_extensions.txt": {60, "无扩展名", "py"},
	}
	for filename, want := range checks {
		data, err := os.ReadFile(c.Path("config/" + filename))
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(data), "\n")
		if len(lines) != want.count || lines[0] != want.first || lines[len(lines)-1] != want.last {
			t.Fatalf("旧默认文件 %s 内容变化: %d 行", filename, len(lines))
		}
	}
	var models map[string][]string
	if err := json.Unmarshal([]byte(defaultFiles["ai_models.json"]), &models); err != nil {
		t.Fatal(err)
	}
	if len(models) != 6 || len(models["qwen"]) != 20 || models["gemini"][0] != "gemini-2.5-pro-exp-03-25" {
		t.Fatal("旧模型菜单不完整")
	}
}

func TestDatabaseURLConflictDoesNotCreateEmptyDatabase(t *testing.T) {
	withoutEnv(t, "API_ID", "USER_ID", "ADMINS", "DEFAULT_TIMEZONE", "DATABASE_URL")
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "db"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "db", "forward.db"), []byte("legacy marker"), 0600); err != nil {
		t.Fatal(err)
	}
	writeEnv(t, root, "DATABASE_URL=sqlite:///other.db\n")
	if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "旧版实际使用") {
		t.Fatalf("数据库位置冲突未明确诊断: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "other.db")); !os.IsNotExist(err) {
		t.Fatal("冲突时创建了空数据库")
	}
}

func TestAdminFallbackAndTimezone(t *testing.T) {
	withoutEnv(t, "API_ID", "USER_ID", "ADMINS", "DEFAULT_TIMEZONE", "DATABASE_URL")
	root := t.TempDir()
	writeEnv(t, root, "USER_ID=90001\nADMINS=\nDEFAULT_TIMEZONE=Asia/Shanghai\n")
	c, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.Admins, []int64{90001}) || c.Timezone.String() != "Asia/Shanghai" {
		t.Fatalf("管理员缺省或时区错误: %+v", c)
	}
	t.Setenv("ADMINS", "90001,bad")
	if _, err := Load(root); err == nil {
		t.Fatal("接受了无效管理员 ID")
	}
}

func TestExistingSessionDoesNotRequireFirstLoginFields(t *testing.T) {
	c := &Config{APIID: 12345, APIHash: "existing", UserID: 90001}
	if err := c.Validate(); err != nil {
		t.Fatalf("已有会话不应要求电话或机器人 token: %v", err)
	}
}

func TestPythonDotEnvCompatibilityFixtures(t *testing.T) {
	var fixtures []struct {
		Name   string            `json:"name"`
		Source string            `json:"source"`
		Env    []string          `json:"env"`
		Want   map[string]string `json:"want"`
	}
	data, err := os.ReadFile(filepath.Join("testdata", "python_dotenv_1_0.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			root := t.TempDir()
			writeEnv(t, root, fixture.Source)
			got, err := readDotEnv(filepath.Join(root, ".env"), fixture.Env)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, fixture.Want) {
				t.Fatalf("与 python-dotenv 1.0.0 不一致\ngot  %#v\nwant %#v", got, fixture.Want)
			}
		})
	}
}
