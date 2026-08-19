#include <windows.h>
#include <tlhelp32.h>

#include <cstdint>
#include <iomanip>
#include <iostream>
#include <string>
#include <vector>

static bool readMemory(HANDLE process, uintptr_t address, void* out, size_t size) {
    SIZE_T read = 0;
    return ReadProcessMemory(process, reinterpret_cast<const void*>(address), out, size, &read) &&
           read == size;
}

static void printBytes(const std::vector<unsigned char>& bytes) {
    for (auto byte : bytes) {
        std::cout << std::hex << std::setw(2) << std::setfill('0')
                  << static_cast<unsigned int>(byte) << ' ';
    }
    std::cout << std::dec << '\n';
}

static uintptr_t findModule(DWORD pid, const wchar_t* wanted, DWORD& size) {
    HANDLE snapshot = CreateToolhelp32Snapshot(TH32CS_SNAPMODULE | TH32CS_SNAPMODULE32, pid);
    if (snapshot == INVALID_HANDLE_VALUE) return 0;

    MODULEENTRY32W entry{};
    entry.dwSize = sizeof(entry);
    uintptr_t base = 0;
    if (Module32FirstW(snapshot, &entry)) {
        do {
            if (_wcsicmp(entry.szModule, wanted) == 0) {
                base = reinterpret_cast<uintptr_t>(entry.modBaseAddr);
                size = entry.modBaseSize;
                break;
            }
        } while (Module32NextW(snapshot, &entry));
    }
    CloseHandle(snapshot);
    return base;
}

int wmain(int argc, wchar_t** argv) {
    if (argc != 2) {
        std::wcerr << L"usage: sg_remote_probe.exe <pid>\n";
        return 2;
    }

    const DWORD pid = static_cast<DWORD>(std::stoul(argv[1]));
    DWORD moduleSize = 0;
    const uintptr_t module = findModule(pid, L"security_guard.node", moduleSize);
    if (module == 0) {
        std::wcerr << L"security_guard.node not found\n";
        return 1;
    }

    HANDLE process = OpenProcess(PROCESS_QUERY_INFORMATION | PROCESS_VM_READ, FALSE, pid);
    if (process == nullptr) {
        std::wcerr << L"OpenProcess failed: " << GetLastError() << L"\n";
        return 1;
    }

    const uintptr_t globalAddress = module + 0x28050;
    uintptr_t object = 0;
    if (!readMemory(process, globalAddress, &object, sizeof(object))) {
        std::cerr << "failed to read factor global\n";
        CloseHandle(process);
        return 1;
    }

    uintptr_t vtable = 0;
    if (object == 0 || !readMemory(process, object, &vtable, sizeof(vtable))) {
        std::cerr << "factor object unavailable\n";
        CloseHandle(process);
        return 1;
    }

    std::cout << "module=" << reinterpret_cast<void*>(module)
              << " size=0x" << std::hex << moduleSize << std::dec << '\n';
    std::cout << "global=0x" << std::hex << globalAddress
              << " object=0x" << object
              << " vtable=0x" << vtable << std::dec << '\n';

    std::vector<uintptr_t> slots(10);
    if (!readMemory(process, vtable, slots.data(), slots.size() * sizeof(uintptr_t))) {
        std::cerr << "failed to read vtable\n";
        CloseHandle(process);
        return 1;
    }
    for (size_t i = 0; i < slots.size(); ++i) {
        std::cout << "slot[" << i << "]=0x" << std::hex << slots[i] << std::dec << '\n';
    }

    for (size_t slot : {size_t(0), size_t(3), size_t(8)}) {
        std::vector<unsigned char> bytes(32);
        if (readMemory(process, slots[slot], bytes.data(), bytes.size())) {
            std::cout << "slot[" << slot << "] bytes: ";
            printBytes(bytes);
        }
    }

    CloseHandle(process);
    return 0;
}
