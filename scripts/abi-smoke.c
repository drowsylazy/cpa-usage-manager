/*
 * abi-smoke.c — 校验 cpa-usage-manager c-shared 产物导出 ABI 符号并能完成一次
 * 完整的 init → call → free 生命周期。
 *
 * 用法：
 *   Linux/macOS: gcc -o abi-smoke abi-smoke.c -ldl && ./abi-smoke ./cpa-usage-manager.so
 *   Windows:     gcc -o abi-smoke.exe abi-smoke.c && ./abi-smoke.exe .\cpa-usage-manager.dll
 *
 * 退出码 0 表示通过，非 0 表示失败。
 */
#include <stdio.h>
#include <stdlib.h>
#include <stdint.h>
#include <string.h>
#include <stddef.h>

#if defined(_WIN32)
#include <windows.h>
typedef HMODULE lib_handle;
static lib_handle lib_open(const char *path) { return LoadLibraryA(path); }
static void *lib_symbol(lib_handle h, const char *name) {
    return (void *)GetProcAddress(h, name);
}
static void lib_close(lib_handle h) { if (h) FreeLibrary(h); }
#else
#include <dlfcn.h>
typedef void *lib_handle;
static lib_handle lib_open(const char *path) { return dlopen(path, RTLD_NOW); }
static void *lib_symbol(lib_handle h, const char *name) { return dlsym(h, name); }
static void lib_close(lib_handle h) { if (h) dlclose(h); }
#endif

/* 与 main.go 中 C 头保持一致的 ABI 结构。 */
typedef struct { void *ptr; size_t len; } cpa_buffer;
typedef struct { uint32_t abi_version; void *host_ctx; void *call; void *free_buffer; } cpa_host_api;
typedef int (*cpa_call_fn)(char *, uint8_t *, size_t, cpa_buffer *);
typedef void (*cpa_free_fn)(void *, size_t);
typedef void (*cpa_shutdown_fn)(void);
typedef struct { uint32_t abi_version; cpa_call_fn call; cpa_free_fn free_buffer; cpa_shutdown_fn shutdown; } cpa_plugin_api;

typedef int (*init_fn)(cpa_host_api *, cpa_plugin_api *);

static int failures = 0;

#define CHECK(cond, msg)                                                       \
    do {                                                                       \
        if (cond) {                                                            \
            printf("ok: %s\n", (msg));                                         \
        } else {                                                               \
            printf("FAIL: %s\n", (msg));                                       \
            failures++;                                                        \
        }                                                                      \
    } while (0)

int main(int argc, char **argv) {
    if (argc < 2) {
        fprintf(stderr, "usage: %s <cpa-usage-manager.{so,dll,dylib}>\n", argv[0]);
        return 2;
    }
    const char *path = argv[1];

    lib_handle lib = lib_open(path);
    CHECK(lib != NULL, "library loads");
    if (!lib) {
#if defined(_WIN32)
        fprintf(stderr, "GetLastError=%lu\n", (unsigned long)GetLastError());
#else
        fprintf(stderr, "%s\n", dlerror());
#endif
        return 1;
    }

    init_fn init = (init_fn)lib_symbol(lib, "cliproxy_plugin_init");
    cpa_call_fn call = (cpa_call_fn)lib_symbol(lib, "cliproxyPluginCall");
    cpa_free_fn free_buf = (cpa_free_fn)lib_symbol(lib, "cliproxyPluginFree");
    cpa_shutdown_fn shutdown = (cpa_shutdown_fn)lib_symbol(lib, "cliproxyPluginShutdown");
    CHECK(init != NULL, "symbol cliproxy_plugin_init exported");
    CHECK(call != NULL, "symbol cliproxyPluginCall exported");
    CHECK(free_buf != NULL, "symbol cliproxyPluginFree exported");
    CHECK(shutdown != NULL, "symbol cliproxyPluginShutdown exported");

    if (!init || !call || !free_buf) {
        lib_close(lib);
        return 1;
    }

    cpa_host_api host = {0};
    cpa_plugin_api plugin;
    memset(&plugin, 0, sizeof(plugin));
    host.abi_version = 1;

    int rc = init(&host, &plugin);
    CHECK(rc == 0, "cliproxy_plugin_init returns 0");
    CHECK(plugin.abi_version == 1, "plugin abi_version == 1");
    CHECK(plugin.call == call, "plugin.call points to cliproxyPluginCall");
    CHECK(plugin.free_buffer == free_buf, "plugin.free_buffer points to cliproxyPluginFree");
    CHECK(plugin.shutdown == shutdown, "plugin.shutdown points to cliproxyPluginShutdown");

    /* 未注册前调用未知方法：期望返回 JSON 错误体，验证 call/free 通路。 */
    char method[] = "smoke.unknown_method";
    cpa_buffer out = {0};
    rc = call(method, NULL, 0, &out);
    CHECK(rc == 0, "call(unknown) returns 0");
    if (out.ptr != NULL && out.len > 0) {
        char *body = (char *)malloc(out.len + 1);
        memcpy(body, out.ptr, out.len);
        body[out.len] = '\0';
        printf("ok: call(unknown) body: %s\n", body);
        int is_json = body[0] == '{' && strstr(body, "\"ok\"") != NULL;
        CHECK(is_json, "call(unknown) returns JSON envelope");
        free(body);
        free_buf(out.ptr, out.len);
        printf("ok: free_buffer releases response\n");
    } else {
        CHECK(0, "call(unknown) produced a response buffer");
    }

    /* 无效 method 指针等空入参不得崩溃（rc 为 1 表示错误且已写入 JSON 错误体，同样合法）。 */
    cpa_buffer out2 = {0};
    rc = call(NULL, NULL, 0, &out2);
    CHECK(rc == 0 || rc == 1, "call(NULL method) returns without crash");
    if (out2.ptr != NULL && out2.len > 0) {
        char *body2 = (char *)malloc(out2.len + 1);
        memcpy(body2, out2.ptr, out2.len);
        body2[out2.len] = '\0';
        printf("ok: call(NULL method) body: %s\n", body2);
        free(body2);
        free_buf(out2.ptr, out2.len);
    }

    /* 重复 init 不崩溃。 */
    cpa_plugin_api plugin2;
    memset(&plugin2, 0, sizeof(plugin2));
    rc = init(&host, &plugin2);
    CHECK(rc == 0, "second cliproxy_plugin_init returns 0");

    if (shutdown) {
        shutdown();
        printf("ok: cliproxyPluginShutdown\n");
    }

    /*
     * 故意不卸载动态库。Go 运行时不支持卸载 c-shared 库：shutdown 之后
     * 运行时线程与定时器仍然存活，FreeLibrary/dlclose 会把它们的代码页抽走，
     * 于是进程在退出前偶发段错误（实测约 1/50，v0.2.1 及更早同样存在）。
     * 宿主 CLIProxyAPI 也从不卸载插件——句柄留到进程退出才是生产行为，
     * 这里保持一致，避免用一个不可能修好的卸载路径把回归测试变成掷硬币。
     * （早退分支仍会卸载：那时还没走完 init，也没什么可保护的。）
     */

    printf(failures ? "ABI SMOKE FAILED (%d)\n" : "ABI SMOKE PASSED\n", failures);
    return failures ? 1 : 0;
}