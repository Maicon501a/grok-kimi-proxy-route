#include <windows.h>

#include <cstdlib>
#include <cstring>
#include <iomanip>
#include <iostream>
#include <string>

using QueryInterfaceFn = int(__fastcall*)(int interface_id, void** out);
using ReadyFn = int(__fastcall*)(void* self);
using StoreStartFn = int(__fastcall*)(void* self, void* buffer, unsigned int capacity);
using FactorsFn = char*(__fastcall*)(void* self, const char* input, int* status);
using FreeFn = void(__fastcall*)(void* self, char* response);

static std::wstring widen(const char* value) {
    if (value == nullptr || *value == '\0') return L".";
    int n = MultiByteToWideChar(CP_UTF8, 0, value, -1, nullptr, 0);
    std::wstring result(static_cast<size_t>(n), L'\0');
    MultiByteToWideChar(CP_UTF8, 0, value, -1, result.data(), n);
    result.pop_back();
    return result;
}

static HMODULE load(const std::wstring& dir, const wchar_t* name) {
    const auto path = dir + L"\\" + name;
    HMODULE module = LoadLibraryW(path.c_str());
    std::wcout << name << L": " << (module != nullptr ? L"loaded" : L"FAILED")
               << L" (" << path << L")\n";
    if (module == nullptr) {
        std::wcout << L"  GetLastError=" << GetLastError() << L"\n";
    }
    return module;
}

static void inspect(HMODULE module, int id) {
    auto query = reinterpret_cast<QueryInterfaceFn>(GetProcAddress(module, "QueryInterface"));
    std::cout << "QueryInterface(" << std::hex << id << std::dec << "): ";
    if (query == nullptr) {
        std::cout << "export missing\n";
        return;
    }

    void* object = nullptr;
    const int interfaceStatus = query(id, &object);
    std::cout << "status=" << interfaceStatus << " object=" << object << "\n";
    if (interfaceStatus == 0 || object == nullptr) return;

    auto vtable = *reinterpret_cast<void***>(object);
    std::cout << "  vtable=" << vtable << "\n";
    for (int i = 0; i < 10; ++i) {
        std::cout << "  [" << i << "]=" << vtable[i] << "\n";
    }

    for (int slot : {0, 3, 8}) {
        HMODULE owner = nullptr;
        GetModuleHandleExW(
            GET_MODULE_HANDLE_EX_FLAG_FROM_ADDRESS |
                GET_MODULE_HANDLE_EX_FLAG_UNCHANGED_REFCOUNT,
            reinterpret_cast<LPCWSTR>(vtable[slot]), &owner);
        wchar_t name[MAX_PATH] = {};
        GetModuleFileNameW(owner, name, MAX_PATH);
        std::wcout << L"  slot[" << slot << L"] owner=" << name
                   << L" base=" << owner << L"\n";
        auto* bytes = reinterpret_cast<const unsigned char*>(vtable[slot]);
        std::cout << "    bytes=";
        for (int i = 0; i < 32; ++i) {
            std::cout << std::hex << std::setw(2) << std::setfill('0')
                      << static_cast<unsigned int>(bytes[i]) << ' ';
        }
        std::cout << std::dec << "\n";
    }

    if (id != 0x129 || std::getenv("SG_CALL") == nullptr) return;

    const auto ready = reinterpret_cast<ReadyFn>(vtable[0]);
    std::cout << "  ready/start status=" << ready(object) << "\n";

    // Start may replace the lazy proxy vtable. The Node binding reloads it
    // before calling the factor method; keep the probe in the same order.
    vtable = *reinterpret_cast<void***>(object);
    std::cout << "  active vtable=" << vtable << "\n";
    for (int i = 0; i < 10; ++i) {
        std::cout << "  active[" << i << "]=" << vtable[i] << "\n";
    }

    const auto factors = reinterpret_cast<FactorsFn>(vtable[8]);
    const auto free_response = reinterpret_cast<FreeFn>(vtable[3]);

    int status = -1;
    const char* input =
        R"({"appkey":"35336201","urlInput":"https://phoenix-gw.alibaba.com/api/adk/llm/generateContent?sg_k=ffffffffffffffffffffffffffffffff"})";
    char* response = factors(object, input, &status);
    std::cout << "  factors response=" << static_cast<void*>(response)
              << " status=" << status << "\n";
    if (response != nullptr) {
        std::cout << "  response length=" << std::strlen(response) << "\n";
        std::cout << "  response=" << response << "\n";
        free_response(object, response);
    }
}

static void bootstrapStaticStore(HMODULE module) {
    auto query = reinterpret_cast<QueryInterfaceFn>(GetProcAddress(module, "QueryInterface"));
    if (query == nullptr) return;

    void* object = nullptr;
    const int status = query(0x110, &object);
    std::cout << "bootstrap QueryInterface(110): status=" << status
              << " object=" << object << "\n";
    if (status == 0 || object == nullptr) return;

    auto vtable = *reinterpret_cast<void***>(object);
    auto start = reinterpret_cast<StoreStartFn>(vtable[0]);
    unsigned char buffer[0x1000] = {};
    const int start_status = start(object, buffer, sizeof(buffer));
    std::cout << "bootstrap store status=" << start_status << "\n";
    std::cout << "bootstrap bytes=";
    for (int i = 0; i < 32; ++i) {
        std::cout << std::hex << std::setw(2) << std::setfill('0')
                  << static_cast<unsigned int>(buffer[i]) << ' ';
    }
    std::cout << std::dec << "\n";
}

int main() {
    const std::wstring dir = widen(std::getenv("SG_DLLS"));
    const auto sdk = load(dir, L"SecurityGuardSDK64.dll");
    load(dir, L"ThreatSieveSDK64.dll");
    const auto safe_path = load(dir, L"AliSafePath64.dll");
    const auto proxy = load(dir, L"AliSafeProxy.dll");
    if (proxy == nullptr) return 1;

    (void)safe_path;
    inspect(proxy, 0x110);
    inspect(proxy, 0x129);
    if (std::getenv("SG_BOOTSTRAP") != nullptr) bootstrapStaticStore(proxy);
    if (sdk != nullptr) {
        std::cout << "SecurityGuardSDK64.dll QueryInterface:\n";
        inspect(sdk, 0x129);
    }
    return 0;
}
