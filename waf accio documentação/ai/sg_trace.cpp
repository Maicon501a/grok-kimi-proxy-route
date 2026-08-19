#include <windows.h>
#include <psapi.h>

#include <algorithm>
#include <cstdint>
#include <filesystem>
#include <fstream>
#include <iomanip>
#include <iostream>
#include <iterator>
#include <sstream>
#include <string>
#include <vector>

struct Breakpoint {
    uintptr_t address = 0;
    unsigned char original = 0;
    bool installed = false;
};

struct SingleStep {
    bool active = false;
    Breakpoint* breakpoint = nullptr;
};

struct HardwareWatch {
    uintptr_t address = 0;
    DWORD tid = 0;
    unsigned int slot = 0;
    const char* label = nullptr;
    bool active = false;
};

struct ModuleRange {
    uintptr_t base = 0;
    size_t size = 0;
    std::wstring name;
};

static uintptr_t g_dispatchObject = 0;
static uintptr_t g_securityGuardBase = 0;
static uintptr_t g_innerBase = 0;
static uintptr_t g_tableBuildObject = 0;
static uintptr_t g_generatorInput = 0;
static bool g_sdkDumped = false;
static bool g_tableStateDumped = false;
static unsigned int g_transformLoopLogs[2] = {};
static unsigned int g_sboxByteLogs = 0;
static uintptr_t g_transformFocusBuffer = 0;
static std::vector<Breakpoint> g_d0WriteBreakpoints;
static std::vector<ModuleRange> g_d0WriteModules;
static std::vector<ModuleRange> g_loadedD0Modules;
static HardwareWatch g_tableWatches[4];
static uintptr_t g_stagingWatchBase = 0;
static uintptr_t g_stagingWatchOffset = 0;
static unsigned int g_stagingWatchHits = 0;

static std::wstring env(const wchar_t* name) {
    const wchar_t* value = _wgetenv(name);
    return value == nullptr ? L"" : value;
}

static std::wstring quote(const std::wstring& value) {
    return L"\"" + value + L"\"";
}

static bool readMemory(HANDLE process, uintptr_t address, void* out, size_t size) {
    SIZE_T read = 0;
    return ReadProcessMemory(process, reinterpret_cast<const void*>(address), out, size, &read) &&
           read == size;
}

static void dumpSdkImage(HANDLE process, const Breakpoint& breakpoint) {
    if (g_sdkDumped || g_securityGuardBase == 0) return;
    const std::wstring dumpPath = env(L"SG_SDK_FULL_DUMP");
    if (dumpPath.empty()) return;

    constexpr size_t imageSize = 0x6f5000;
    std::vector<unsigned char> image(imageSize);
    if (!readMemory(process, g_securityGuardBase, image.data(), image.size())) {
        std::cout << "[trace] sdk full dump failed\n";
        return;
    }
    if (breakpoint.address >= g_securityGuardBase) {
        const uintptr_t offset = breakpoint.address - g_securityGuardBase;
        if (offset < image.size()) image[static_cast<size_t>(offset)] = breakpoint.original;
    }
    std::ofstream dump(std::filesystem::path(dumpPath), std::ios::binary);
    dump.write(reinterpret_cast<const char*>(image.data()),
               static_cast<std::streamsize>(image.size()));
    if (dump.good()) {
        g_sdkDumped = true;
        std::cout << "[trace] sdk full dump="
                  << std::string(dumpPath.begin(), dumpPath.end())
                  << " bytes=" << image.size() << "\n";
    }
}

static bool writeByte(HANDLE process, uintptr_t address, unsigned char value) {
    DWORD oldProtect = 0;
    if (!VirtualProtectEx(process, reinterpret_cast<void*>(address), 1,
                          PAGE_EXECUTE_READWRITE, &oldProtect)) {
        return false;
    }
    SIZE_T written = 0;
    const bool ok = WriteProcessMemory(process, reinterpret_cast<void*>(address),
                                       &value, 1, &written) && written == 1;
    FlushInstructionCache(process, reinterpret_cast<const void*>(address), 1);
    DWORD ignored = 0;
    VirtualProtectEx(process, reinterpret_cast<void*>(address), 1, oldProtect, &ignored);
    return ok;
}

static bool arm(HANDLE process, Breakpoint& breakpoint) {
    if (breakpoint.installed) return true;
    if (!readMemory(process, breakpoint.address, &breakpoint.original, 1)) return false;
    if (!writeByte(process, breakpoint.address, 0xcc)) return false;
    breakpoint.installed = true;
    return true;
}

static bool ensurePhysicalBreakpoint(HANDLE process, Breakpoint& breakpoint) {
    if (!breakpoint.installed) return arm(process, breakpoint);
    unsigned char current = 0;
    if (!readMemory(process, breakpoint.address, &current, sizeof(current))) return false;
    if (current == 0xcc) return true;
    // Some SDK entrypoints are unpacked after DLL load. Preserve the
    // materialized instruction instead of restoring the loader stub later.
    breakpoint.original = current;
    return writeByte(process, breakpoint.address, 0xcc);
}

static bool disarm(HANDLE process, Breakpoint& breakpoint) {
    if (!breakpoint.installed) return true;
    if (!writeByte(process, breakpoint.address, breakpoint.original)) return false;
    breakpoint.installed = false;
    return true;
}

static bool ensureBreakpoint(HANDLE process, std::vector<Breakpoint>& breakpoints,
                             uintptr_t address) {
    if (address == 0) return false;
    for (auto& breakpoint : breakpoints) {
        if (breakpoint.address == address) {
            return breakpoint.installed || arm(process, breakpoint);
        }
    }
    Breakpoint breakpoint;
    breakpoint.address = address;
    if (!arm(process, breakpoint)) return false;
    breakpoints.push_back(breakpoint);
    return true;
}

static void printBytes(HANDLE process, uintptr_t address, size_t count);

static bool executableAddress(HANDLE process, uintptr_t address) {
    MEMORY_BASIC_INFORMATION info{};
    if (VirtualQueryEx(process, reinterpret_cast<const void*>(address), &info,
                       sizeof(info)) != sizeof(info)) {
        return false;
    }
    const DWORD executable = info.Protect & 0xff;
    return executable == PAGE_EXECUTE || executable == PAGE_EXECUTE_READ ||
           executable == PAGE_EXECUTE_READWRITE || executable == PAGE_EXECUTE_WRITECOPY;
}

static uintptr_t contextRegister(const CONTEXT& context, unsigned int index) {
    switch (index) {
        case 0: return context.Rax;
        case 1: return context.Rcx;
        case 2: return context.Rdx;
        case 3: return context.Rbx;
        case 4: return context.Rsp;
        case 5: return context.Rbp;
        case 6: return context.Rsi;
        case 7: return context.Rdi;
        case 8: return context.R8;
        case 9: return context.R9;
        case 10: return context.R10;
        case 11: return context.R11;
        case 12: return context.R12;
        case 13: return context.R13;
        case 14: return context.R14;
        case 15: return context.R15;
        default: return 0;
    }
}

static bool looksLikeTableData(HANDLE process, uintptr_t address) {
    if (address == 0 || address >= 0x0000ffffffffffffULL) return false;
    uintptr_t header[4] = {};
    if (!readMemory(process, address + 0x80, header, sizeof(header))) return false;
    size_t readablePointers = 0;
    for (uintptr_t value : header) {
        if (value >= 0x0000010000000000ULL && value < 0x0000ffffffffffffULL) {
            ++readablePointers;
        }
    }
    if (readablePointers < 2) return false;

    std::vector<unsigned char> sbox(256);
    if (!readMemory(process, address + 0xa0, sbox.data(), sbox.size())) return false;
    bool seen[256] = {};
    size_t lowBytes = 0;
    size_t distinct = 0;
    for (unsigned char value : sbox) {
        if (value <= 0x7f) ++lowBytes;
        if (!seen[value]) {
            seen[value] = true;
            ++distinct;
        }
    }
    return lowBytes >= 220 && distinct >= 48;
}

static uintptr_t d0ProbeModuleBase(uintptr_t address) {
    for (const auto& module : g_d0WriteModules) {
        if (address >= module.base && address < module.base + module.size) {
            return module.base;
        }
    }
    return g_securityGuardBase;
}

static void armD0WriteBreakpoints(HANDLE process,
                                  const std::vector<ModuleRange>& modules) {
    if (env(L"SG_TRACE_D0_WRITES") != L"1") return;
    for (auto& breakpoint : g_d0WriteBreakpoints) {
        if (breakpoint.installed) disarm(process, breakpoint);
    }
    g_d0WriteBreakpoints.clear();
    g_d0WriteModules = modules;

    size_t candidates = 0;
    for (const auto& module : modules) {
        if (module.base == 0 || module.size < 8 || module.size > 0x10000000) continue;
        std::vector<unsigned char> image(module.size);
        if (!readMemory(process, module.base, image.data(), image.size())) continue;
        for (size_t offset = 0; offset + 7 < image.size(); ++offset) {
            const unsigned char rex = image[offset];
            if (rex < 0x48 || rex > 0x4f || (rex & 0x08) == 0 ||
                image[offset] == 0xcc || image[offset + 1] != 0x89) {
                continue;
            }
            const unsigned char modrm = image[offset + 2];
            if ((modrm >> 6) != 2 || image[offset + 3] != 0xd0 ||
                image[offset + 4] != 0x00 || image[offset + 5] != 0x00 ||
                image[offset + 6] != 0x00) {
                continue;
            }
            const unsigned int baseRegister = (modrm & 0x07u) +
                                              ((rex & 0x01u) ? 8u : 0u);
            if (baseRegister == 4 || baseRegister == 5) continue;
            const uintptr_t address = module.base + offset;
            if (!executableAddress(process, address)) continue;

            Breakpoint breakpoint;
            breakpoint.address = address;
            if (arm(process, breakpoint)) {
                g_d0WriteBreakpoints.push_back(breakpoint);
                ++candidates;
            }
        }
    }
    std::cout << "[trace] d0-write probes armed=" << candidates << "\n";
}

static void logD0Write(HANDLE process, const CONTEXT& context,
                       const Breakpoint& breakpoint, uintptr_t base) {
    unsigned char instruction[7] = {};
    instruction[0] = breakpoint.original;
    if (!readMemory(process, breakpoint.address + 1, instruction + 1, sizeof(instruction) - 1)) {
        return;
    }
    const unsigned char rex = instruction[0];
    const unsigned char modrm = instruction[2];
    const unsigned int baseRegister = (modrm & 0x07u) + ((rex & 0x01u) ? 8u : 0u);
    const unsigned int sourceRegister = ((modrm >> 3) & 0x07u) +
                                        ((rex & 0x04u) ? 8u : 0u);
    const uintptr_t destination = contextRegister(context, baseRegister) + 0xd0;
    const uintptr_t source = contextRegister(context, sourceRegister);
    const bool tableCandidate = looksLikeTableData(process, source);
    if (!tableCandidate && env(L"SG_TRACE_D0_ALL") != L"1") return;

    uintptr_t previous = 0;
    readMemory(process, destination, &previous, sizeof(previous));
    const uintptr_t rva = breakpoint.address - base;
    std::cout << "[trace] d0-write rva=0x" << std::hex << rva
              << " dest=0x" << destination
              << " source=0x" << source
              << " previous=0x" << previous
              << " base-reg=" << std::dec << baseRegister
              << " source-reg=" << sourceRegister
              << " table-candidate=" << (tableCandidate ? "yes" : "no") << "\n";
    if (tableCandidate) {
        std::cout << "[trace] d0-write source sbox: ";
        printBytes(process, source + 0xa0, 32);
    }
}

static void restoreBreakpointsInDump(uintptr_t start, std::vector<unsigned char>& code,
                                     const std::vector<Breakpoint>& breakpoints) {
    for (const auto& breakpoint : breakpoints) {
        if (!breakpoint.installed || breakpoint.address < start) continue;
        const uintptr_t offset = breakpoint.address - start;
        if (offset < code.size()) code[static_cast<size_t>(offset)] = breakpoint.original;
    }
}

static std::wstring moduleName(HANDLE process, void* base) {
    wchar_t buffer[MAX_PATH] = {};
    const DWORD n = GetModuleFileNameExW(process, static_cast<HMODULE>(base), buffer,
                                         static_cast<DWORD>(std::size(buffer)));
    return n == 0 ? L"" : std::wstring(buffer, n);
}

static bool isD0TraceModule(const std::wstring& name) {
    std::wstring lower = name;
    std::transform(lower.begin(), lower.end(), lower.begin(), towlower);
    return lower.find(L"securityguardsdk64") != std::wstring::npos ||
           lower.find(L"alisafeproxy") != std::wstring::npos ||
           lower.find(L"threatsievesdk64") != std::wstring::npos ||
           lower.find(L"alisafepath64") != std::wstring::npos ||
           lower.find(L"report.dll") != std::wstring::npos ||
           lower.find(L"netcore.dll") != std::wstring::npos ||
           lower.find(L"security_guard.node") != std::wstring::npos;
}

static size_t remoteImageSize(HANDLE process, uintptr_t base) {
    DWORD peOffset = 0;
    if (!readMemory(process, base + 0x3c, &peOffset, sizeof(peOffset))) return 0;
    DWORD imageSize = 0;
    if (!readMemory(process, base + peOffset + 4 + 20 + 0x38,
                    &imageSize, sizeof(imageSize))) return 0;
    return static_cast<size_t>(imageSize);
}

static std::vector<ModuleRange> traceD0Modules(HANDLE) {
    return g_loadedD0Modules;
}

static std::wstring fileNameFromHandle(HANDLE file) {
    if (file == nullptr || file == INVALID_HANDLE_VALUE) return L"";
    wchar_t buffer[4096] = {};
    const DWORD n = GetFinalPathNameByHandleW(file, buffer,
                                              static_cast<DWORD>(std::size(buffer)),
                                              FILE_NAME_NORMALIZED);
    return n == 0 ? L"" : std::wstring(buffer, n);
}

static bool endsWithInsensitive(std::wstring value, const wchar_t* suffix) {
    std::transform(value.begin(), value.end(), value.begin(), towlower);
    std::wstring target(suffix);
    std::transform(target.begin(), target.end(), target.begin(), towlower);
    return value.size() >= target.size() &&
           value.compare(value.size() - target.size(), target.size(), target) == 0;
}

static void printBytes(HANDLE process, uintptr_t address, size_t count) {
    std::vector<unsigned char> bytes(count);
    if (!readMemory(process, address, bytes.data(), bytes.size())) {
        std::cout << "<unreadable>\n";
        return;
    }
    for (unsigned char byte : bytes) {
        std::cout << std::hex << std::setw(2) << std::setfill('0')
                  << static_cast<unsigned int>(byte) << ' ';
    }
    std::cout << std::dec << "\n";
}

static void printAscii(HANDLE process, uintptr_t address, size_t count) {
    std::vector<char> bytes(count + 1, '\0');
    if (!readMemory(process, address, bytes.data(), count)) {
        std::cout << "ascii=<unreadable>\n";
        return;
    }
    std::cout << "ascii=";
    for (size_t i = 0; i < count && bytes[i] != '\0'; ++i) {
        const unsigned char c = static_cast<unsigned char>(bytes[i]);
        std::cout << ((c >= 0x20 && c <= 0x7e) ? static_cast<char>(c) : '.');
    }
    std::cout << "\n";
}

static void printRvaStringObject(HANDLE process, uintptr_t base, uintptr_t rva) {
    uintptr_t address = base + rva;
    uintptr_t words[4] = {};
    if (!readMemory(process, address, words, sizeof(words))) {
        std::cout << "[trace] sdk string 0x" << std::hex << rva
                  << " <unreadable>\n" << std::dec;
        return;
    }
    const size_t size = static_cast<size_t>(words[2]);
    const size_t capacity = static_cast<size_t>(words[3]);
    uintptr_t textAddress = address;
    if (capacity >= 16 && words[0] != 0) textAddress = words[0];
    std::cout << "[trace] sdk string 0x" << std::hex << rva
              << " size=" << std::dec << size
              << " cap=" << capacity
              << " text=";
    printAscii(process, textAddress, std::min<size_t>(size, 128));
}

static void logTableState(HANDLE process, const char* label, uintptr_t object) {
    uintptr_t tableData = 0;
    if (object == 0 || !readMemory(process, object + 0xd0, &tableData, sizeof(tableData))) {
        std::cout << "[trace] " << label << " table-data=<unreadable>\n";
        return;
    }
    std::cout << "[trace] " << label << " table-data=0x" << std::hex
              << tableData << std::dec << "\n";
    if (tableData == 0 || tableData >= 0x0000ffffffffffffULL) return;
    std::cout << "[trace] " << label << " sbox+0xa0: ";
    printBytes(process, tableData + 0xa0, 256);

    const std::wstring dumpPath = env(L"SG_TABLE_OBJECT_DUMP");
    if (!dumpPath.empty() && !g_tableStateDumped) {
        std::vector<unsigned char> objectBytes(0x200);
        std::vector<unsigned char> tableBytes(0x200);
        const bool objectOk = readMemory(process, object, objectBytes.data(), objectBytes.size());
        const bool tableOk = readMemory(process, tableData, tableBytes.data(), tableBytes.size());
        if (objectOk && tableOk) {
            std::ofstream objectDump(std::filesystem::path(dumpPath + L".object"),
                                     std::ios::binary);
            std::ofstream tableDump(std::filesystem::path(dumpPath + L".table"),
                                    std::ios::binary);
            objectDump.write(reinterpret_cast<const char*>(objectBytes.data()),
                             static_cast<std::streamsize>(objectBytes.size()));
            tableDump.write(reinterpret_cast<const char*>(tableBytes.data()),
                            static_cast<std::streamsize>(tableBytes.size()));
            g_tableStateDumped = objectDump.good() && tableDump.good();
            if (g_tableStateDumped) {
                std::cout << "[trace] table object dump="
                          << std::string(dumpPath.begin(), dumpPath.end())
                          << " bytes=" << objectBytes.size() << "+"
                          << tableBytes.size() << "\n";
            }
        }
    }
}

static void logTableSlotState(HANDLE process, const char* label, uintptr_t object) {
    uintptr_t tableSlot = 0;
    if (object == 0 || !readMemory(process, object + 0xd0, &tableSlot, sizeof(tableSlot))) {
        std::cout << "[trace] " << label << " table-slot=<unreadable>\n";
        return;
    }
    uintptr_t tableData = 0;
    if (tableSlot != 0) readMemory(process, tableSlot, &tableData, sizeof(tableData));
    std::cout << "[trace] " << label << " table-slot=0x" << std::hex
              << tableSlot << " table-data=0x" << tableData << std::dec << "\n";
    if (tableData == 0 || tableData >= 0x0000ffffffffffffULL) return;
    std::cout << "[trace] " << label << " sbox+0xa0: ";
    printBytes(process, tableData + 0xa0, 256);
}

static bool contextFor(DWORD tid, CONTEXT& context) {
    HANDLE thread = OpenThread(THREAD_GET_CONTEXT | THREAD_SET_CONTEXT, FALSE, tid);
    if (thread == nullptr) return false;
    context.ContextFlags = CONTEXT_FULL;
    const bool ok = GetThreadContext(thread, &context) != FALSE;
    if (ok) context.ContextFlags = CONTEXT_FULL;
    CloseHandle(thread);
    return ok;
}

static bool setContextFor(DWORD tid, CONTEXT& context) {
    HANDLE thread = OpenThread(THREAD_GET_CONTEXT | THREAD_SET_CONTEXT, FALSE, tid);
    if (thread == nullptr) return false;
    const bool ok = SetThreadContext(thread, &context) != FALSE;
    CloseHandle(thread);
    return ok;
}

static bool setHardwareWatch(DWORD tid, unsigned int slot, uintptr_t address,
                             const char* label, unsigned int lengthCode = 2) {
    if (slot >= 4 || address == 0 || lengthCode > 3) return false;
    const uintptr_t alignment = lengthCode == 0 ? 1 : lengthCode == 1 ? 2 :
                                lengthCode == 3 ? 4 : 8;
    if ((address % alignment) != 0) return false;
    HANDLE thread = OpenThread(THREAD_GET_CONTEXT | THREAD_SET_CONTEXT, FALSE, tid);
    if (thread == nullptr) return false;
    CONTEXT context{};
    context.ContextFlags = CONTEXT_DEBUG_REGISTERS;
    const bool readOk = GetThreadContext(thread, &context) != FALSE;
    if (!readOk) {
        CloseHandle(thread);
        return false;
    }

    switch (slot) {
    case 0: context.Dr0 = address; break;
    case 1: context.Dr1 = address; break;
    case 2: context.Dr2 = address; break;
    case 3: context.Dr3 = address; break;
    }
    const DWORD64 slotMask = 0x3ull << (slot * 2);
    const DWORD64 conditionMask = 0xfull << (16 + slot * 4);
    context.Dr7 &= ~(slotMask | conditionMask);
    context.Dr7 |= (1ull << (slot * 2));
    context.Dr7 |= (1ull << (16 + slot * 4));
    context.Dr7 |= (static_cast<DWORD64>(lengthCode) << (18 + slot * 4));
    context.Dr6 = 0;
    context.ContextFlags = CONTEXT_DEBUG_REGISTERS;
    const bool writeOk = SetThreadContext(thread, &context) != FALSE;
    CloseHandle(thread);
    if (!writeOk) return false;

    g_tableWatches[slot] = HardwareWatch{address, tid, slot, label, true};
    return true;
}

static void clearHardwareWatch(const HardwareWatch& watch) {
    if (!watch.active || watch.slot >= 4) return;
    HANDLE thread = OpenThread(THREAD_GET_CONTEXT | THREAD_SET_CONTEXT, FALSE, watch.tid);
    if (thread == nullptr) return;
    CONTEXT context{};
    context.ContextFlags = CONTEXT_DEBUG_REGISTERS;
    if (GetThreadContext(thread, &context)) {
        context.Dr7 &= ~(0x3ull << (watch.slot * 2));
        context.Dr7 &= ~(0xfull << (16 + watch.slot * 4));
        switch (watch.slot) {
        case 0: context.Dr0 = 0; break;
        case 1: context.Dr1 = 0; break;
        case 2: context.Dr2 = 0; break;
        case 3: context.Dr3 = 0; break;
        }
        context.Dr6 &= ~(1ull << watch.slot);
        SetThreadContext(thread, &context);
    }
    CloseHandle(thread);
}

static void clearTableWatches() {
    for (auto& watch : g_tableWatches) {
        if (watch.active) clearHardwareWatch(watch);
        watch = HardwareWatch{};
    }
}

static void armTableWatches(HANDLE process, DWORD tid, uintptr_t object) {
    if (env(L"SG_TRACE_HW_WATCH") != L"1") return;
    clearTableWatches();
    if (object == 0) return;

    uintptr_t tableData = 0;
    readMemory(process, object + 0xd0, &tableData, sizeof(tableData));
    std::cout << "[trace] table watches object=0x" << std::hex << object
              << " field=0x" << object + 0xd0
              << " value=0x" << tableData << std::dec << "\n";
    if (!setHardwareWatch(tid, 0, object + 0xd0, "operator+d0")) {
        std::cout << "[trace] table watch operator+d0 failed\n";
    }
    if (tableData != 0 && tableData < 0x0000ffffffffffffULL) {
        const uintptr_t sbox = tableData + 0xa0;
        if (!setHardwareWatch(tid, 1, sbox, "table+sbox")) {
            std::cout << "[trace] table watch table+sbox failed\n";
        }
    }
}

static void armTableSboxWatch(HANDLE, DWORD tid, uintptr_t tableData) {
    if (env(L"SG_TRACE_HW_WATCH") != L"1" || tableData == 0 ||
        tableData >= 0x0000ffffffffffffULL) {
        return;
    }
    if (g_tableWatches[1].active) clearHardwareWatch(g_tableWatches[1]);
    const uintptr_t sbox = tableData + 0xa0;
    if (setHardwareWatch(tid, 1, sbox, "table+sbox")) {
        std::cout << "[trace] table+sbox watch armed address=0x"
                  << std::hex << sbox << std::dec << "\n";
    } else {
        std::cout << "[trace] table+sbox watch failed address=0x"
                  << std::hex << sbox << std::dec << "\n";
    }
}

static bool handleHardwareWatch(HANDLE process, DWORD tid, const CONTEXT& context) {
    HANDLE thread = OpenThread(THREAD_GET_CONTEXT, FALSE, tid);
    if (thread == nullptr) return false;
    CONTEXT debug{};
    debug.ContextFlags = CONTEXT_DEBUG_REGISTERS;
    const bool debugOk = GetThreadContext(thread, &debug) != FALSE;
    CloseHandle(thread);
    if (!debugOk || debug.Dr6 == 0) return false;
    bool handled = false;
    bool rearmStaging = false;
    uintptr_t nextStagingAddress = 0;
    for (auto& watch : g_tableWatches) {
        if (!watch.active || watch.tid != tid || (debug.Dr6 & (1ull << watch.slot)) == 0) {
            continue;
        }
        uintptr_t value = 0;
        readMemory(process, watch.address, &value, sizeof(value));
        std::cout << "[trace] table-watch label=" << watch.label
                  << " address=0x" << std::hex << watch.address
                  << " rip=0x" << context.Rip
                  << " value=0x" << value << std::dec << "\n";
        if (watch.label != nullptr && std::string(watch.label) == "table+sbox") {
            std::cout << "[trace] table-watch sbox: ";
            printBytes(process, watch.address, 64);
        }
        if (watch.label != nullptr && std::string(watch.label) == "generator+c8") {
            uintptr_t returnAddress = 0;
            readMemory(process, context.Rsp, &returnAddress, sizeof(returnAddress));
            std::cout << "[trace] generator+c8 return=0x" << std::hex
                      << returnAddress << std::dec << "\n";
            std::cout << "[trace] generator+c8 surrounding: ";
            printBytes(process, watch.address - 0x10, 0x30);
        }
        if (watch.label != nullptr && std::string(watch.label) == "transform+14") {
            std::cout << "[trace] transform+14 post: ";
            printBytes(process, watch.address, 0x20);
            std::cout << "[trace] transform+14 surrounding: ";
            printBytes(process, watch.address - 0x10, 0x30);
        }
        if (watch.label != nullptr &&
            (std::string(watch.label) == "transform-261040" ||
             std::string(watch.label) == "transform-2651e0")) {
            const uintptr_t buffer = watch.address - 0x65;
            std::cout << "[trace] " << watch.label << " post: ";
            printBytes(process, buffer, 0x66);
        }
        if (watch.label != nullptr && std::string(watch.label) == "staging-write") {
            std::cout << "[trace] staging-write offset=0x" << std::hex
                      << g_stagingWatchOffset << " rip=0x" << context.Rip
                      << " bytes: ";
            printBytes(process, g_stagingWatchBase + g_stagingWatchOffset, 8);
            ++g_stagingWatchHits;
            if (g_stagingWatchHits < 40 && g_stagingWatchBase != 0) {
                g_stagingWatchOffset += 8;
                nextStagingAddress = (g_stagingWatchBase + g_stagingWatchOffset) &
                                     ~static_cast<uintptr_t>(7);
                rearmStaging = true;
            }
        }
        clearHardwareWatch(watch);
        watch.active = false;
        handled = true;
    }
    if (rearmStaging) {
        if (!setHardwareWatch(tid, 2, nextStagingAddress, "staging-write")) {
            std::cout << "[trace] staging-write rearm failed address=0x"
                      << std::hex << nextStagingAddress << std::dec << "\n";
        }
    }
    return handled;
}

static void logMethodEntry(HANDLE process, DWORD tid, const CONTEXT& context) {
    std::cout << "[trace] method-entry tid=" << tid
              << " rcx=0x" << std::hex << context.Rcx
              << " rdx=0x" << context.Rdx
              << " r8=0x" << context.R8
              << " r9=0x" << context.R9
              << " rsp=0x" << context.Rsp << std::dec << "\n";
    uintptr_t returnAddress = 0;
    if (readMemory(process, context.Rsp, &returnAddress, sizeof(returnAddress))) {
        std::cout << "[trace] method return=0x" << std::hex << returnAddress
                  << std::dec << "\n";
    }
    std::vector<uintptr_t> stack(8);
    if (readMemory(process, context.Rsp, stack.data(), stack.size() * sizeof(uintptr_t))) {
        std::cout << "[trace] method stack=";
        for (auto value : stack) std::cout << "0x" << std::hex << value << ' ';
        std::cout << std::dec << "\n";
    }
    std::cout << "[trace] rdx bytes: ";
    printBytes(process, context.Rdx, 32);
    printAscii(process, context.Rdx, 256);
    int pointedStatus = -1;
    if (readMemory(process, context.R8, &pointedStatus, sizeof(pointedStatus))) {
        std::cout << "[trace] *r8 status=" << pointedStatus << "\n";
    } else {
        std::cout << "[trace] r8 is not readable as length pointer\n";
    }
    uintptr_t innerObject = 0;
    uintptr_t innerVtable = 0;
    uintptr_t innerFactors = 0;
    if (readMemory(process, context.Rcx + 8, &innerObject, sizeof(innerObject)) &&
        readMemory(process, innerObject, &innerVtable, sizeof(innerVtable)) &&
        readMemory(process, innerVtable + 8 * sizeof(uintptr_t), &innerFactors, sizeof(innerFactors))) {
        std::cout << "[trace] inner object=0x" << std::hex << innerObject
                  << " vtable=0x" << innerVtable
                  << " slot8=0x" << innerFactors << std::dec << "\n";
        logTableState(process, "method-entry", innerObject);
        if (g_securityGuardBase != 0 && innerFactors >= g_securityGuardBase) {
            std::cout << "[trace] inner slot8 RVA=0x" << std::hex
                      << (innerFactors - g_securityGuardBase) << std::dec << "\n";
        }
        std::cout << "[trace] inner slot8 bytes: ";
        printBytes(process, innerFactors, 32);
    }
}

static void logCallReturn(HANDLE process, DWORD tid, const CONTEXT& context) {
    int status = -1;
    const bool statusOk = readMemory(process, context.Rsp + 0x50, &status, sizeof(status));
    std::cout << "[trace] call-return tid=" << tid
              << " rax=0x" << std::hex << context.Rax
              << " rsp=0x" << context.Rsp << std::dec
              << " status=" << (statusOk ? std::to_string(status) : "<unreadable>") << "\n";
    std::vector<uintptr_t> stack(12);
    if (readMemory(process, context.Rsp, stack.data(), stack.size() * sizeof(uintptr_t))) {
        std::cout << "[trace] return stack=";
        for (auto value : stack) std::cout << "0x" << std::hex << value << ' ';
        std::cout << std::dec << "\n";
    }
    uintptr_t activeVtable = 0;
    if (readMemory(process, g_dispatchObject, &activeVtable, sizeof(activeVtable))) {
        uintptr_t activeFree = 0;
        uintptr_t activeFactors = 0;
        readMemory(process, activeVtable + 3 * sizeof(uintptr_t), &activeFree, sizeof(activeFree));
        readMemory(process, activeVtable + 8 * sizeof(uintptr_t), &activeFactors, sizeof(activeFactors));
        std::cout << "[trace] proxy after-call vtable=0x" << std::hex << activeVtable
                  << " slot3=0x" << activeFree
                  << " slot8=0x" << activeFactors << std::dec << "\n";
    }
    if (context.Rax != 0) {
        std::cout << "[trace] response object bytes: ";
        printBytes(process, context.Rax, 96);
        std::cout << "[trace] response object ascii: ";
        printAscii(process, context.Rax, 4096);
    }
}

static void logCallEntry(HANDLE process, DWORD tid, const CONTEXT& context) {
    g_dispatchObject = context.Rcx;
    std::cout << "[trace] call-entry tid=" << tid
              << " rcx=0x" << std::hex << context.Rcx
              << " rdx=0x" << context.Rdx
              << " r8=0x" << context.R8
              << " r9=0x" << context.R9
              << " rsp=0x" << context.Rsp << std::dec << "\n";
    uintptr_t returnAddress = 0;
    if (readMemory(process, context.Rsp, &returnAddress, sizeof(returnAddress))) {
        std::cout << "[trace] call return=0x" << std::hex << returnAddress << std::dec << "\n";
    }
    uintptr_t vtable = 0;
    uintptr_t slot8 = 0;
    if (readMemory(process, context.Rcx, &vtable, sizeof(vtable)) &&
        readMemory(process, vtable + 8 * sizeof(uintptr_t), &slot8, sizeof(slot8))) {
        std::cout << "[trace] dispatch vtable=0x" << std::hex << vtable
                  << " slot8=0x" << slot8 << std::dec << "\n";
        std::cout << "[trace] slot8 bytes: ";
        printBytes(process, slot8, 32);
    }
    int status = -1;
    if (readMemory(process, context.R8, &status, sizeof(status))) {
        std::cout << "[trace] call *r8 status=" << status << "\n";
    }
    std::cout << "[trace] input object bytes: ";
    printBytes(process, context.Rdx, 128);
    std::cout << "[trace] input object ascii: ";
    printAscii(process, context.Rdx, 256);
    uintptr_t innerObject = 0;
    if (readMemory(process, context.Rcx + 8, &innerObject, sizeof(innerObject))) {
        logTableState(process, "call-entry", innerObject);
    }
}

static void logInitEntry(HANDLE process, DWORD tid, const CONTEXT& context,
                         uintptr_t base, const Breakpoint& breakpoint,
                         const char* label) {
    const uintptr_t rva = breakpoint.address >= base ? breakpoint.address - base : 0;
    std::cout << "[trace] " << label
              << " tid=" << tid
              << " rva=0x" << std::hex << rva
              << " rcx=0x" << context.Rcx
              << " rdx=0x" << context.Rdx
              << " r8=0x" << context.R8
              << " r9=0x" << context.R9
              << " rax=0x" << context.Rax
              << " rsp=0x" << context.Rsp << std::dec << "\n";

    uintptr_t returnAddress = 0;
    if (readMemory(process, context.Rsp, &returnAddress, sizeof(returnAddress))) {
        std::cout << "[trace] " << label << " return=0x"
                  << std::hex << returnAddress << std::dec << "\n";
    }

    std::vector<uintptr_t> stack(12);
    if (readMemory(process, context.Rsp, stack.data(), stack.size() * sizeof(uintptr_t))) {
        std::cout << "[trace] " << label << " stack=";
        for (auto value : stack) std::cout << "0x" << std::hex << value << ' ';
        std::cout << std::dec << "\n";
    }

    if (std::string(label) == "init-callback") {
        std::cout << "[trace] init-callback rdx bytes: ";
        printBytes(process, context.Rdx, 64);
    }
    if (std::string(label) == "sdk-initUMID") {
        const std::wstring dumpPath = env(L"SG_SDK_INIT_DUMP");
        if (!dumpPath.empty()) {
            std::vector<unsigned char> code(0x4000);
            if (readMemory(process, breakpoint.address, code.data(), code.size())) {
                code[0] = breakpoint.original;
                std::ofstream dump(std::filesystem::path(dumpPath), std::ios::binary);
                dump.write(reinterpret_cast<const char*>(code.data()),
                           static_cast<std::streamsize>(code.size()));
                std::cout << "[trace] SDK initUMID dump="
                          << std::string(dumpPath.begin(), dumpPath.end())
                          << " bytes=" << code.size() << "\n";
            }
        }
    }
}

static void logInitDispatch(HANDLE process, DWORD tid, const CONTEXT& context,
                            uintptr_t nodeBase, const Breakpoint& breakpoint,
                            std::vector<Breakpoint>& initTargets) {
    const uintptr_t rva = breakpoint.address - nodeBase;
    uintptr_t target = 0;
    if (context.Rax != 0) {
        readMemory(process, context.Rax + sizeof(uintptr_t), &target, sizeof(target));
    }

    std::cout << "[trace] init-dispatch tid=" << tid
              << " rva=0x" << std::hex << rva
              << " object=0x" << context.Rcx
              << " vtable=0x" << context.Rax
              << " env=0x" << context.Rdx
              << " callback=0x" << context.R8
              << " target=0x" << target << std::dec << "\n";
    std::cout << "[trace] init-dispatch vtable bytes: ";
    printBytes(process, context.Rax, 32);
    if (target != 0) {
        std::cout << "[trace] init-dispatch target bytes: ";
        printBytes(process, target, 64);
        const bool armed = ensureBreakpoint(process, initTargets, target);
        std::cout << "[trace] init-dispatch target breakpoint="
                  << (armed ? "armed" : "failed") << "\n";
        const std::wstring dumpPath = env(L"SG_INIT_TARGET_DUMP");
        if (!dumpPath.empty()) {
            std::vector<unsigned char> code(0x10000);
            if (readMemory(process, target, code.data(), code.size())) {
                code[0] = breakpoint.original;
                std::ofstream dump(std::filesystem::path(dumpPath), std::ios::binary);
                dump.write(reinterpret_cast<const char*>(code.data()),
                           static_cast<std::streamsize>(code.size()));
                std::cout << "[trace] init target dump="
                          << std::string(dumpPath.begin(), dumpPath.end())
                          << " bytes=" << code.size() << "\n";
            }
        }
    }
}

static void logInitNativeCall(HANDLE process, DWORD tid, const CONTEXT& context,
                              const Breakpoint& breakpoint,
                              std::vector<Breakpoint>& initNativeTargets,
                              Breakpoint& alreadyArmedBreakpoint) {
    uintptr_t target = context.Rax;
    std::cout << "[trace] init-native-call tid=" << tid
              << " site=0x" << std::hex << breakpoint.address
              << " rcx=0x" << context.Rcx
              << " rdx=0x" << context.Rdx
              << " r8=0x" << context.R8
              << " r9=0x" << context.R9
              << " target=0x" << target << std::dec << "\n";
    if (target == 0) return;
    std::cout << "[trace] init-native target bytes: ";
    printBytes(process, target, 64);
    if (target == alreadyArmedBreakpoint.address) {
        std::cout << "[trace] init-native target breakpoint="
                  << (ensurePhysicalBreakpoint(process, alreadyArmedBreakpoint)
                          ? "already-armed" : "failed") << "\n";
        return;
    }
    const bool armed = ensureBreakpoint(process, initNativeTargets, target);
    std::cout << "[trace] init-native target breakpoint="
              << (armed ? "armed" : "failed") << "\n";
}

static void logInnerEntry(HANDLE process, DWORD tid, const CONTEXT& context,
                          const Breakpoint& breakpoint) {
    g_innerBase = breakpoint.address - 0xe1130;
    dumpSdkImage(process, breakpoint);
    std::cout << "[trace] inner-entry tid=" << tid
              << " rip=0x" << std::hex << context.Rip
              << " rcx=0x" << std::hex << context.Rcx
              << " rdx=0x" << context.Rdx
              << " r8=0x" << context.R8
              << " r9=0x" << context.R9
              << " rsp=0x" << context.Rsp << std::dec << "\n";
    std::cout << "[trace] inner input ascii: ";
    printAscii(process, context.Rdx, 2048);
    logTableState(process, "inner-entry", context.Rcx);
    if (g_innerBase != 0) {
        for (uintptr_t rva : {0x763830, 0x763849, 0x7638df, 0x7638e8,
                              0x7638ed, 0x7638f6, 0x763907, 0x763922,
                              0x763950, 0x76395e, 0x763967, 0x76397e}) {
            printRvaStringObject(process, g_innerBase, rva);
        }
    }
    std::cout << "[trace] inner code bytes: ";
    printBytes(process, context.Rip - 1, 128);

    const std::wstring dumpPath = env(L"SG_TRACE_DUMP");
    if (!dumpPath.empty()) {
        std::vector<unsigned char> code(0x4000);
        if (readMemory(process, breakpoint.address, code.data(), code.size())) {
            code[0] = breakpoint.original;
            std::ofstream dump(std::filesystem::path(dumpPath), std::ios::binary);
            dump.write(reinterpret_cast<const char*>(code.data()),
                       static_cast<std::streamsize>(code.size()));
            std::cout << "[trace] inner code dump=" << std::string(dumpPath.begin(), dumpPath.end())
                      << " bytes=" << code.size() << "\n";
        }
    }
}

static void logParseStart(HANDLE process, DWORD tid, const CONTEXT& context) {
    std::cout << "[trace] parse-start tid=" << tid
              << " rip=0x" << std::hex << context.Rip
              << " rcx=0x" << context.Rcx
              << " rdx=0x" << context.Rdx
              << " r8=0x" << context.R8
              << " r9=0x" << context.R9 << std::dec << "\n";
    if (g_innerBase != 0) {
        for (uintptr_t rva : {0x763830, 0x763849, 0x7638df, 0x7638e8,
                              0x7638ed, 0x7638f6, 0x763907, 0x763922,
                              0x763950, 0x76395e, 0x763967, 0x76397e}) {
            std::cout << "[trace] sdk pool 0x" << std::hex << rva << std::dec << ' ';
            printAscii(process, g_innerBase + rva, 96);
        }
    }
    uintptr_t savedInput = 0;
    if (readMemory(process, context.Rbp + 0x120, &savedInput, sizeof(savedInput))) {
        std::cout << "[trace] parse input ptr=0x" << std::hex << savedInput << std::dec << "\n";
        std::cout << "[trace] parse input ascii: ";
        printAscii(process, savedInput, 2048);
    }
}

static void logFieldLookup(HANDLE process, DWORD tid, const CONTEXT& context) {
    std::cout << "[trace] field-lookup tid=" << tid
              << " rip=0x" << std::hex << context.Rip
              << " rcx=0x" << context.Rcx
              << " rdx=0x" << context.Rdx
              << " r8=0x" << context.R8
              << " r9=0x" << context.R9
              << " rsp=0x" << context.Rsp << std::dec << "\n";
    std::cout << "[trace] field key: ";
    printAscii(process, context.Rdx, 128);
    uintptr_t returnAddress = 0;
    if (readMemory(process, context.Rsp, &returnAddress, sizeof(returnAddress))) {
        std::cout << "[trace] field return=0x" << std::hex << returnAddress << std::dec << "\n";
    }
}

static void logFieldReturn(HANDLE process, DWORD tid, const CONTEXT& context) {
    std::cout << "[trace] field-return tid=" << tid
              << " rip=0x" << std::hex << context.Rip
              << " rax=0x" << context.Rax
              << " rdx=0x" << context.Rdx
              << " r8=0x" << context.R8
              << " r9=0x" << context.R9 << std::dec << "\n";
    if (context.Rax != 0) {
        std::cout << "[trace] field result bytes: ";
        printBytes(process, context.Rax, 64);
        std::cout << "[trace] field result ascii: ";
        printAscii(process, context.Rax, 256);
        uintptr_t words[8] = {};
        if (readMemory(process, context.Rax, words, sizeof(words))) {
            for (size_t i = 0; i < std::size(words); ++i) {
                if (words[i] < 0x10000 || words[i] > 0x0000ffffffffffffULL) continue;
                std::cout << "[trace] field ptr+0x" << std::hex << (i * 8)
                          << "=0x" << words[i] << std::dec << ' ';
                printAscii(process, words[i], 128);
            }
        }
    }
}

static void logGeneratorInputCandidates(HANDLE process, const char* label) {
    if (g_generatorInput == 0) return;

    std::cout << "[trace] " << label << " candidate records:" << "\n";
    for (size_t offset = 0x68; offset < 0x100; offset += sizeof(uintptr_t)) {
        uintptr_t raw = 0;
        if (!readMemory(process, g_generatorInput + offset, &raw, sizeof(raw))) break;

        const uintptr_t low48 = raw & 0x0000ffffffffffffULL;
        const unsigned int high16 = static_cast<unsigned int>(raw >> 48);
        unsigned char sample[16] = {};
        const bool readable = low48 != 0 && low48 < 0x0000ffffffffffffULL &&
                              readMemory(process, low48, sample, sizeof(sample));
        std::cout << "[trace] " << label << " +0x" << std::hex << offset
                  << " raw=0x" << raw
                  << " low48=0x" << low48
                  << " high16=0x" << high16
                  << " readable=" << (readable ? "yes" : "no")
                  << std::dec;
        if (readable) {
            std::cout << " sample=";
            for (unsigned char byte : sample) {
                std::cout << std::hex << std::setw(2) << std::setfill('0')
                          << static_cast<unsigned int>(byte) << ' ';
            }
            std::cout << std::dec;
        }
        std::cout << "\n";
    }
}

static void logGeneratorInputBuffers(HANDLE process, const char* label) {
    if (g_generatorInput == 0) return;

    struct BufferField {
        size_t offset;
        const char* name;
        size_t maximum;
    };
    static constexpr BufferField fields[] = {
        {0x00, "buffer-00", 256},
        {0x28, "buffer-28", 256},
        {0x88, "buffer-88", 128},
        {0xd0, "buffer-d0", 256},
    };

    for (const auto& field : fields) {
        uintptr_t buffer = 0;
        uintptr_t length = 0;
        if (!readMemory(process, g_generatorInput + field.offset,
                        &buffer, sizeof(buffer)) ||
            !readMemory(process, g_generatorInput + field.offset + sizeof(uintptr_t),
                        &length, sizeof(length))) {
            continue;
        }
        if (buffer == 0 || buffer >= 0x0000ffffffffffffULL) continue;

        const size_t count = std::min<size_t>(field.maximum, length);
        std::cout << "[trace] " << label << ' ' << field.name
                  << " ptr=0x" << std::hex << buffer
                  << " length=0x" << length << std::dec
                  << " bytes: ";
        printBytes(process, buffer, count);
        std::cout << "[trace] " << label << ' ' << field.name << " ascii: ";
        printAscii(process, buffer, count);
    }
}

static void armGeneratorC8Watch(HANDLE process, DWORD tid) {
    if (env(L"SG_TRACE_GENERATOR_C8") != L"1" || g_generatorInput == 0) return;
    if (g_tableWatches[2].active) clearHardwareWatch(g_tableWatches[2]);

    const uintptr_t address = g_generatorInput + 0xc8;
    if (setHardwareWatch(tid, 2, address, "generator+c8")) {
        std::cout << "[trace] generator+c8 watch armed address=0x"
                  << std::hex << address << " tid=" << tid << std::dec << "\n";
    } else {
        std::cout << "[trace] generator+c8 watch failed address=0x"
                  << std::hex << address << std::dec << "\n";
    }
}

static void armTransform14Watch(DWORD tid, const CONTEXT& context) {
    if (env(L"SG_TRACE_TRANSFORM_WRITES") != L"1" || context.Rdx == 0 ||
        (context.Rdx & 7u) != 0) {
        return;
    }
    if (g_tableWatches[3].active) clearHardwareWatch(g_tableWatches[3]);
    if (setHardwareWatch(tid, 3, context.Rdx, "transform+14")) {
        std::cout << "[trace] transform+14 watch armed address=0x"
                  << std::hex << context.Rdx << " tid=" << tid << std::dec << "\n";
    } else {
        std::cout << "[trace] transform+14 watch failed address=0x"
                  << std::hex << context.Rdx << std::dec << "\n";
    }
}

static void armTransformFinalWatch(DWORD tid, uintptr_t buffer, uintptr_t length,
                                   const char* label) {
    if (env(L"SG_TRACE_TRANSFORM_FINAL") != L"1" || buffer == 0 || length == 0) {
        return;
    }
    if (g_tableWatches[3].active) clearHardwareWatch(g_tableWatches[3]);
    const uintptr_t address = buffer + length - 1;
    if (setHardwareWatch(tid, 3, address, label, 0)) {
        std::cout << "[trace] " << label << " watch armed address=0x"
                  << std::hex << address << " buffer=0x" << buffer
                  << " length=0x" << length << " tid=" << tid << std::dec << "\n";
    } else {
        std::cout << "[trace] " << label << " watch failed address=0x"
                  << std::hex << address << " buffer=0x" << buffer
                  << " length=0x" << length << std::dec << "\n";
    }
}

static void armStagingWriteWatch(HANDLE process, DWORD tid, uintptr_t object,
                                 uintptr_t offset) {
    if (env(L"SG_TRACE_STAGING_WRITES") != L"1" || object == 0) {
        return;
    }

    uintptr_t field = 0;
    uintptr_t buffer = 0;
    if (!readMemory(process, object + 0xd0, &field, sizeof(field)) || field == 0 ||
        !readMemory(process, field + 0x98, &buffer, sizeof(buffer)) || buffer == 0) {
        std::cout << "[trace] staging-write base=<unreadable> object=0x"
                  << std::hex << object << std::dec << "\n";
        return;
    }

    if (g_tableWatches[2].active) clearHardwareWatch(g_tableWatches[2]);
    const uintptr_t target = buffer + offset;
    g_stagingWatchBase = target;
    g_stagingWatchOffset = 0;
    g_stagingWatchHits = 0;
    const uintptr_t address = target & ~static_cast<uintptr_t>(7);
    if (setHardwareWatch(tid, 2, address, "staging-write")) {
        std::cout << "[trace] staging-write watch armed address=0x"
                  << std::hex << address << " scratch=0x" << buffer
                  << " offset=0x" << offset << " target=0x" << target
                  << " field=0x" << field << " object=0x" << object
                  << " tid=" << tid << std::dec << "\n";
    } else {
        std::cout << "[trace] staging-write watch failed address=0x"
                  << std::hex << address << " scratch=0x" << buffer
                  << " offset=0x" << offset << " target=0x" << target
                  << " object=0x" << object << std::dec << "\n";
        g_stagingWatchBase = 0;
    }
}

static void logGeneratorReturn(HANDLE process, DWORD tid, const CONTEXT& context,
                               const Breakpoint& breakpoint) {
    const uintptr_t rva = g_securityGuardBase != 0 &&
                                  breakpoint.address >= g_securityGuardBase
                              ? breakpoint.address - g_securityGuardBase
                              : 0;
    std::cout << "[trace] generator-return rva=0x" << std::hex << rva
              << " tid=" << tid
              << " rax=0x" << context.Rax
              << " rdx=0x" << context.Rdx
              << " r8=0x" << context.R8
              << " r9=0x" << context.R9
              << " rsp=0x" << context.Rsp << std::dec << "\n";
    if (context.Rax != 0 && context.Rax < 0x0000ffffffffffffULL) {
        logTableSlotState(process, "generator-return", context.Rax);
        std::cout << "[trace] generator-return object bytes: ";
        printBytes(process, context.Rax, 256);
    }
    if (g_generatorInput != 0) {
        std::cout << "[trace] generator-input-after ptr=0x" << std::hex
                  << g_generatorInput << std::dec << " bytes: ";
        printBytes(process, g_generatorInput, 512);
        std::cout << "[trace] generator-input-after inline+0xd0 ascii: ";
        printAscii(process, g_generatorInput + 0xd0, 128);
        logGeneratorInputCandidates(process, "generator-input-after");
    }
    armGeneratorC8Watch(process, tid);
}

static void logTableBuildReturn(HANDLE process, DWORD tid, const CONTEXT& context,
                                const Breakpoint& breakpoint) {
    const uintptr_t rva = g_securityGuardBase != 0 &&
                                  breakpoint.address >= g_securityGuardBase
                              ? breakpoint.address - g_securityGuardBase
                              : 0;
    std::cout << "[trace] table-build-return rva=0x" << std::hex << rva
              << " tid=" << tid
              << " rax=0x" << context.Rax
              << " rdx=0x" << context.Rdx
              << " r8=0x" << context.R8
              << " r9=0x" << context.R9
              << " rsp=0x" << context.Rsp << std::dec << "\n";
    logTableState(process, "table-build-return", g_tableBuildObject);
    if (g_tableBuildObject != 0) {
        std::cout << "[trace] table-build object bytes: ";
        printBytes(process, g_tableBuildObject, 256);
    }
}

static void logDispatchSite(HANDLE process, uintptr_t base, const CONTEXT& context,
                            const Breakpoint& breakpoint) {
    static unsigned int sboxDispatchLogs = 0;
    const uintptr_t rva = breakpoint.address >= base ? breakpoint.address - base : 0;
    if (rva == 0x28e13a && sboxDispatchLogs++ >= 16) return;
    uintptr_t target = 0;
    uintptr_t table = 0;
    uintptr_t index = 0;
    const char* kind = "dispatch";

    switch (rva) {
    case 0x1bb55b:
        target = context.R9;
        kind = "wrapper-init";
        break;
    case 0x1bb5c8:
        table = context.Rsi;
        index = context.Rbp;
        kind = "wrapper-call";
        break;
    case 0x1bb69a:
    case 0x1bb6b3:
        table = context.Rdi;
        index = context.Rax;
        kind = rva == 0x1bb69a ? "state-init" : "state-call";
        break;
    case 0x28e13a:
        table = context.Rcx;
        index = context.Rax;
        kind = "sbox-byte-generator";
        break;
    case 0x28e88d:
        table = context.Rsi;
        index = context.Rax;
        kind = "sbox-seed-source";
        break;
    case 0x28e89e:
        table = context.Rsi;
        index = context.Rdx;
        kind = "sbox-seed-dispatch";
        break;
    default:
        break;
    }

    if (target == 0 && table != 0) {
        readMemory(process, table + index * sizeof(uintptr_t), &target, sizeof(target));
    }
    std::cout << "[trace] dispatch-site rva=0x" << std::hex << rva
              << " kind=" << kind
              << " table=0x" << table
              << " index=0x" << index
              << " target=0x" << target;
    uintptr_t returnAddress = 0;
    if (readMemory(process, context.Rsp, &returnAddress, sizeof(returnAddress))) {
        std::cout << " return=0x" << returnAddress;
    }
    std::cout << std::dec << "\n";
    if (target != 0 && target < 0x0000ffffffffffffULL) {
        const uintptr_t targetRva = target >= base ? target - base : 0;
        std::cout << "[trace] dispatch-target rva=0x" << std::hex << targetRva
                  << " bytes: ";
        printBytes(process, target, 96);
    }
}

static void logTableInitCall(HANDLE process, DWORD tid, const CONTEXT& context) {
    std::cout << "[trace] table-init-call tid=" << tid
              << " rcx=0x" << std::hex << context.Rcx
              << " rdi=0x" << context.Rdi
              << " rdx=0x" << context.Rdx
              << " r8=0x" << context.R8
              << " r9=0x" << context.R9 << std::dec << "\n";
    armTableWatches(process, tid, context.Rdi);
}

static void logTableFieldStore(HANDLE process, DWORD tid, const CONTEXT& context) {
    const uintptr_t destination = context.R15 + 0xd0;
    uintptr_t previous = 0;
    readMemory(process, destination, &previous, sizeof(previous));
    const bool tableCandidate = looksLikeTableData(process, context.Rax);
    std::cout << "[trace] table-field-store tid=" << tid
              << " object=0x" << std::hex << context.R15
              << " dest=0x" << destination
              << " source=0x" << context.Rax
              << " previous=0x" << previous
              << " table-candidate=" << (tableCandidate ? "yes" : "no")
              << std::dec << "\n";
    if (context.Rax != 0 && context.Rax < 0x0000ffffffffffffULL) {
        std::cout << "[trace] table-field source bytes: ";
        printBytes(process, context.Rax, 320);
        armTableSboxWatch(process, tid, context.Rax);
    }
}

static void logSboxByteStore(HANDLE process, DWORD tid, const CONTEXT& context,
                             const Breakpoint& breakpoint) {
    uintptr_t destination = 0;
    uintptr_t loopIndex = 0;
    uintptr_t state = 0;
    uintptr_t stateA = 0;
    uintptr_t stateB = 0;
    unsigned int rawValue = 0;
    readMemory(process, context.Rsp + 0x60, &destination, sizeof(destination));
    readMemory(process, context.Rsp + 0x390, &loopIndex, sizeof(loopIndex));
    readMemory(process, context.Rsp + 0x28, &state, sizeof(state));
    readMemory(process, context.Rsp + 0x24, &stateA, sizeof(stateA));
    readMemory(process, context.Rsp + 0x2c, &stateB, sizeof(stateB));
    readMemory(process, context.Rsp + 0x160, &rawValue, sizeof(rawValue));
    if (g_sboxByteLogs++ < 256) {
        const uintptr_t rva = breakpoint.address >= g_securityGuardBase
                                  ? breakpoint.address - g_securityGuardBase
                                  : 0;
        std::cout << "[trace] sbox-byte-store rva=0x" << std::hex << rva
                  << " tid=" << tid
                  << " index=0x" << context.Rsi
                  << " stack-index=0x" << loopIndex
                  << " value=0x" << (context.Rax & 0xff)
                  << " raw=0x" << rawValue
                  << " dest=0x" << destination
                  << " state=0x" << state
                  << " state-a=0x" << stateA
                  << " state-b=0x" << stateB
                  << " rsp=0x" << context.Rsp << std::dec << "\n";
    }
}

static void logSboxRandState(HANDLE process, DWORD tid, const CONTEXT& context,
                             const Breakpoint& breakpoint) {
    static unsigned int logs = 0;
    if (logs++ >= 256) return;
    unsigned int oldState = 0;
    readMemory(process, context.Rax + 0x28, &oldState, sizeof(oldState));
    const unsigned int nextState = static_cast<unsigned int>(context.Rcx);
    const unsigned int result = (nextState >> 16) & 0x7fff;
    const uintptr_t rva = breakpoint.address >= g_securityGuardBase
                              ? breakpoint.address - g_securityGuardBase
                              : 0;
    std::cout << "[trace] sbox-rand rva=0x" << std::hex << rva
              << " tid=" << tid
              << " state=0x" << oldState
              << " next=0x" << nextState
              << " result=0x" << result
              << " state-object=0x" << context.Rax << std::dec << "\n";
}

static void logSboxRandSeed(HANDLE process, DWORD tid, const CONTEXT& context,
                            const Breakpoint& breakpoint) {
    unsigned int currentState = 0;
    uintptr_t returnAddress = 0;
    readMemory(process, context.Rax + 0x28, &currentState, sizeof(currentState));
    readMemory(process, context.Rsp + 0x28, &returnAddress, sizeof(returnAddress));
    const uintptr_t rva = breakpoint.address >= g_securityGuardBase
                              ? breakpoint.address - g_securityGuardBase
                              : 0;
    std::cout << "[trace] sbox-rand-seed rva=0x" << std::hex << rva
              << " tid=" << tid
              << " seed=0x" << (context.Rbx & 0xffffffffu)
              << " state-before=0x" << currentState
              << " state-object=0x" << context.Rax
              << " return=0x" << returnAddress << std::dec << "\n";
}

static void logSboxSeedSource(DWORD tid, const CONTEXT& context,
                              const Breakpoint& breakpoint) {
    const uintptr_t rva = breakpoint.address >= g_securityGuardBase
                              ? breakpoint.address - g_securityGuardBase
                              : 0;
    std::cout << "[trace] sbox-seed-source-return rva=0x" << std::hex << rva
              << " tid=" << tid
              << " value=0x" << context.Rax
              << " rcx=0x" << context.Rcx
              << " rdx=0x" << context.Rdx
              << " r8=0x" << context.R8
              << " r9=0x" << context.R9
              << " rsp=0x" << context.Rsp << std::dec << "\n";
}

static void logHelperEntry(HANDLE process, DWORD tid, const CONTEXT& context,
                           uintptr_t base, const Breakpoint& breakpoint,
                           Breakpoint& returnBreakpoint,
                           bool& returnOverlapsSignExtract,
                           Breakpoint& tableBuildReturn) {
    const uintptr_t rva = breakpoint.address >= base ? breakpoint.address - base : 0;
    if (rva == 0x112a10) {
        g_generatorInput = context.Rdx;
        armGeneratorC8Watch(process, tid);
    }
    if (rva == 0x1bb010) {
        armStagingWriteWatch(process, tid, context.Rcx, context.Rdx + 4);
    }
    if (rva == 0x2651e0 && context.R8 == 0x14 &&
        env(L"SG_TRACE_TRANSFORM_FOCUS") == L"1") {
        g_transformFocusBuffer = context.Rdx;
        armTransform14Watch(tid, context);
    }
    if ((rva == 0x261040 || rva == 0x2651e0) && context.R8 == 0x66) {
        armTransformFinalWatch(tid, context.Rdx, context.R8,
                               rva == 0x261040 ? "transform-261040" :
                                                  "transform-2651e0");
        uintptr_t state = 0;
        if (readMemory(process, context.Rsp + 0x38, &state, sizeof(state))) {
            std::cout << "[trace] transform rsp+38=0x" << std::hex << state
                      << " (low=0x" << (state & 0xff) << ")"
                      << " rdx=0x" << context.Rdx
                      << " (rdx low=0x" << (context.Rdx & 0xff) << ")"
                      << std::dec << "\n";
        }
    }
    std::cout << "[trace] helper-entry rva=0x" << std::hex << rva
              << " tid=" << tid
              << " rip=0x" << context.Rip
              << " rcx=0x" << context.Rcx
              << " rdx=0x" << context.Rdx
              << " r8=0x" << context.R8
              << " r9=0x" << context.R9
              << " rsp=0x" << context.Rsp << std::dec << "\n";
    uintptr_t returnAddress = 0;
    if (readMemory(process, context.Rsp, &returnAddress, sizeof(returnAddress))) {
        std::cout << "[trace] helper return=0x" << std::hex << returnAddress << std::dec << "\n";
    }
    if (rva == 0x30ae0 || rva == 0xb57e0 || rva == 0x112a10 ||
        rva == 0x33440 || rva == 0xdf480 || rva == 0xed330 || rva == 0x8fc10) {
        std::cout << "[trace] helper rdx ascii: ";
        printAscii(process, context.Rdx, 256);
        std::cout << "[trace] helper r8 ascii: ";
        printAscii(process, context.R8, 256);
    }
    if (rva == 0x30ae0 && context.R8 != 0) {
        uintptr_t data = 0;
        uintptr_t label = 0;
        if (readMemory(process, context.R8 + 0x20, &data, sizeof(data))) {
            std::cout << "[trace] header value data=0x" << std::hex << data << std::dec << ' ';
            printAscii(process, data, 2048);
        }
        if (readMemory(process, context.R8 + 0x30, &label, sizeof(label))) {
            std::cout << "[trace] header value label=0x" << std::hex << label << std::dec << ' ';
            printAscii(process, label, 128);
        }
    }
    if (rva == 0x112a10) {
        std::cout << "[trace] generator input bytes: ";
        printBytes(process, context.Rdx, 512);
        std::cout << "[trace] generator input inline+0xd0 ascii: ";
        printAscii(process, context.Rdx + 0xd0, 128);
        logGeneratorInputCandidates(process, "generator-input-before");
        logGeneratorInputBuffers(process, "generator-input-before");
        logTableSlotState(process, "generator-input", context.Rdx);
        if (returnBreakpoint.installed) disarm(process, returnBreakpoint);
        uintptr_t returnAddress = 0;
        if (readMemory(process, context.Rsp, &returnAddress, sizeof(returnAddress))) {
            returnBreakpoint.address = returnAddress;
            if (returnAddress == base + 0xe2b46) {
                returnOverlapsSignExtract = true;
                std::cout << "[trace] generator-return overlaps sign-extract at 0x"
                          << std::hex << returnAddress << std::dec << "\n";
            } else {
                returnOverlapsSignExtract = false;
                const bool armed = arm(process, returnBreakpoint);
                std::cout << "[trace] generator-return breakpoint="
                          << (armed ? "armed" : "failed") << " at 0x"
                          << std::hex << returnAddress << std::dec << "\n";
            }
        }
        const std::wstring dumpPath = env(L"SG_HELPER_DUMP");
        if (!dumpPath.empty()) {
            std::vector<unsigned char> code(0x8000);
            if (readMemory(process, breakpoint.address, code.data(), code.size())) {
                code[0] = breakpoint.original;
                std::ofstream dump(std::filesystem::path(dumpPath), std::ios::binary);
                dump.write(reinterpret_cast<const char*>(code.data()),
                           static_cast<std::streamsize>(code.size()));
                std::cout << "[trace] generator code dump="
                          << std::string(dumpPath.begin(), dumpPath.end())
                          << " bytes=" << code.size() << "\n";
            }
        }
    }
    if (rva == 0x261040 || rva == 0x2651e0) {
        std::cout << "[trace] transform input rva=0x" << std::hex << rva
                  << " ptr=0x" << context.Rdx
                  << " rcx=0x" << context.Rcx
                  << " r8=0x" << context.R8
                  << " r9=0x" << context.R9 << std::dec << " bytes: ";
        printBytes(process, context.Rdx, 128);
        uintptr_t contextTable = 0;
        if (readMemory(process, context.Rcx + 0xd0, &contextTable,
                       sizeof(contextTable))) {
            std::cout << "[trace] transform ctx+0xd0=0x" << std::hex
                      << contextTable << std::dec << "\n";
            if (contextTable != 0) {
                std::cout << "[trace] transform table bytes: ";
                printBytes(process, contextTable, 192);
                uintptr_t tableData = 0;
                if (readMemory(process, contextTable, &tableData, sizeof(tableData)) &&
                    tableData != 0) {
                    std::cout << "[trace] transform table-data=0x" << std::hex
                              << tableData << std::dec << "\n";
                    std::cout << "[trace] transform sbox+0xa0: ";
                    printBytes(process, tableData + 0xa0, 256);
                }
            }
        }
    }
    if (rva == 0x261372 || rva == 0x2654e0) {
        const size_t slot = rva == 0x261372 ? 0 : 1;
        uintptr_t focusedBuffer = 0;
        uintptr_t focusedRemaining = 0;
        readMemory(process, context.Rsp + 0x50, &focusedBuffer, sizeof(focusedBuffer));
        readMemory(process, context.Rsp + 0x58, &focusedRemaining, sizeof(focusedRemaining));
        const bool focused = slot == 1 && g_transformFocusBuffer != 0 &&
                             focusedBuffer >= g_transformFocusBuffer &&
                             focusedBuffer < g_transformFocusBuffer + 0x20;
        if (focused || g_transformLoopLogs[slot]++ < 64) {
            uintptr_t tableSlot = 0;
            uintptr_t tableData = 0;
            uintptr_t buffer = 0;
            uintptr_t offset = 0;
            uintptr_t remaining = 0;
            readMemory(process, context.Rsp + 0x40, &tableSlot, sizeof(tableSlot));
            readMemory(process, context.Rsp + 0x48, &offset, sizeof(offset));
            buffer = focusedBuffer;
            remaining = focusedRemaining;
            if (tableSlot != 0) readMemory(process, tableSlot, &tableData, sizeof(tableData));
            std::cout << "[trace] transform-loop rva=0x" << std::hex << rva
                      << " focused=" << (focused ? "yes" : "no")
                      << " table-slot=0x" << tableSlot
                      << " table-data=0x" << tableData
                      << " offset=0x" << offset
                      << " buffer=0x" << buffer
                      << " remaining=0x" << remaining
                      << " rsp+38=";
            uintptr_t word = 0;
            if (readMemory(process, context.Rsp + 0x38, &word, sizeof(word))) {
                std::cout << "0x" << word;
            } else {
                std::cout << "<unreadable>";
            }
            std::cout << std::dec << "\n";
            if (tableData != 0 && tableData < 0x0000ffffffffffffULL) {
                std::cout << "[trace] transform-loop sbox+0xa0: ";
                printBytes(process, tableData + 0xa0, 256);
                if (g_transformLoopLogs[slot] == 1) {
                    const std::wstring sboxRoot = env(L"SG_SBOX_DUMP");
                    if (!sboxRoot.empty()) {
                        std::wstring suffix = slot == 0 ? L".261040" : L".2651e0";
                        std::vector<unsigned char> sbox(256);
                        if (readMemory(process, tableData + 0xa0, sbox.data(), sbox.size())) {
                            std::ofstream dump(std::filesystem::path(sboxRoot + suffix),
                                               std::ios::binary);
                            dump.write(reinterpret_cast<const char*>(sbox.data()),
                                       static_cast<std::streamsize>(sbox.size()));
                        }
                    }
                    const std::wstring tableRoot = env(L"SG_TABLE_DUMP");
                    if (!tableRoot.empty()) {
                        std::wstring suffix = slot == 0 ? L".261040" : L".2651e0";
                        std::vector<unsigned char> table(0x200);
                        if (readMemory(process, tableData, table.data(), table.size())) {
                            std::ofstream dump(std::filesystem::path(tableRoot + suffix),
                                               std::ios::binary);
                            dump.write(reinterpret_cast<const char*>(table.data()),
                                       static_cast<std::streamsize>(table.size()));
                        }
                    }
                }
            }
            std::cout << "[trace] transform-loop buffer bytes: ";
            printBytes(process, buffer, remaining <= 128 ? remaining : 128);
        }
    }
    if (rva == 0x1bbe70) {
        const std::wstring dumpPath = env(L"SG_CRYPTO_DUMP");
        if (!dumpPath.empty()) {
            std::vector<unsigned char> code(0x10000);
            if (readMemory(process, breakpoint.address, code.data(), code.size())) {
                code[0] = breakpoint.original;
                std::ofstream dump(std::filesystem::path(dumpPath), std::ios::binary);
                dump.write(reinterpret_cast<const char*>(code.data()),
                           static_cast<std::streamsize>(code.size()));
                std::cout << "[trace] crypto code dump="
                          << std::string(dumpPath.begin(), dumpPath.end())
                          << " bytes=" << code.size() << "\n";
            }
        }
    }
    if (rva == 0x1bb4e0) {
        g_tableBuildObject = context.Rcx;
        logTableState(process, "table-build-entry", g_tableBuildObject);
        const std::wstring dumpPath = env(L"SG_TABLE_BUILD_DUMP");
        if (!dumpPath.empty()) {
            std::vector<unsigned char> code(0x4000);
            if (readMemory(process, breakpoint.address, code.data(), code.size())) {
                code[0] = breakpoint.original;
                std::ofstream dump(std::filesystem::path(dumpPath), std::ios::binary);
                dump.write(reinterpret_cast<const char*>(code.data()),
                           static_cast<std::streamsize>(code.size()));
                std::cout << "[trace] table-build code dump="
                          << std::string(dumpPath.begin(), dumpPath.end())
                          << " bytes=" << code.size() << "\n";
            }
        }
        if (tableBuildReturn.installed) disarm(process, tableBuildReturn);
        uintptr_t returnAddress = 0;
        if (readMemory(process, context.Rsp, &returnAddress, sizeof(returnAddress))) {
            tableBuildReturn.address = returnAddress;
            const bool armed = arm(process, tableBuildReturn);
            std::cout << "[trace] table-build-return breakpoint="
                      << (armed ? "armed" : "failed") << " at 0x"
                      << std::hex << returnAddress << std::dec << "\n";
        }
    }
}

static void logSignExtract(HANDLE process, DWORD tid, const CONTEXT& context) {
    uintptr_t material = 0;
    unsigned int length = 0;
    readMemory(process, context.Rbp + 0x3d8, &material, sizeof(material));
    readMemory(process, context.Rbp + 0x3e0, &length, sizeof(length));
    std::cout << "[trace] sign-extract tid=" << tid
              << " rip=0x" << std::hex << context.Rip
              << " eax=0x" << static_cast<unsigned int>(context.Rax & 0xffffffffu)
              << " r13=0x" << context.R13
              << " material=0x" << material
              << " length=0x" << length << std::dec << " (" << length << ")\n";
    if (material != 0) {
        std::cout << "[trace] sign material ascii: ";
        printAscii(process, material, std::min<unsigned int>(length, 4096));
        std::cout << "[trace] sign material bytes: ";
        printBytes(process, material, std::min<unsigned int>(length, 128));
    }
}

static void logOperatorEntry(HANDLE process, DWORD tid, const CONTEXT& context,
                             const Breakpoint& breakpoint, uintptr_t base,
                             std::vector<Breakpoint>& operatorSites) {
    const uintptr_t rva = breakpoint.address - base;
    std::cout << "[trace] operator-entry rva=0x" << std::hex << rva
              << " tid=" << tid
              << " rcx=0x" << context.Rcx
              << " rdx=0x" << context.Rdx
              << " r8=0x" << context.R8
              << " r9=0x" << context.R9
              << " rsp=0x" << context.Rsp << std::dec << "\n";
    uintptr_t returnAddress = 0;
    if (readMemory(process, context.Rsp, &returnAddress, sizeof(returnAddress))) {
        std::cout << "[trace] operator return=0x" << std::hex << returnAddress
                  << std::dec << "\n";
    }
    std::cout << "[trace] operator rdx bytes: ";
    printBytes(process, context.Rdx, 128);
    if (rva == 0x264960 && context.R8 == 0x66) {
        std::cout << "[trace] op-264960 pre buffer: ";
        printBytes(process, context.Rdx, 0x66);
        if (env(L"SG_TRACE_STAGING_WRITES") == L"1" && context.Rdx != 0) {
            if (g_tableWatches[2].active) clearHardwareWatch(g_tableWatches[2]);
            g_stagingWatchBase = context.Rdx;
            g_stagingWatchOffset = 0;
            g_stagingWatchHits = 0;
            const uintptr_t addr = context.Rdx & ~static_cast<uintptr_t>(7);
            if (!setHardwareWatch(tid, 2, addr, "staging-write")) {
                std::cout << "[trace] staging-write rearm2 failed address=0x"
                          << std::hex << addr << std::dec << "\n";
            } else {
                std::cout << "[trace] staging-write rearm2 address=0x"
                          << std::hex << addr << std::dec << "\n";
            }
        }
    }
    if (rva == 0x2648a0 && context.R8 == 0x66) {
        std::cout << "[trace] op-2648a0 pre buffer: ";
        printBytes(process, context.Rdx, 0x66);
    }
    if (rva == 0x264960 && context.R9 != 0 &&
        context.R9 < 0x0000ffffffffffffULL) {
        std::cout << "[trace] operator r9 bytes: ";
        printBytes(process, context.R9, 64);
        std::cout << "[trace] operator r9 ascii: ";
        printAscii(process, context.R9, 64);
    }
    logTableState(process, "operator-entry", context.Rcx);

    const std::vector<uintptr_t>* offsets = nullptr;
    static const std::vector<uintptr_t> firstOperatorSites = {0x71, 0xa1};
    static const std::vector<uintptr_t> secondOperatorSites = {0x66, 0x86};
    if (rva == 0x2648a0) offsets = &firstOperatorSites;
    if (rva == 0x264960) offsets = &secondOperatorSites;
    if (offsets != nullptr) {
        for (uintptr_t offset : *offsets) {
            ensureBreakpoint(process, operatorSites, breakpoint.address + offset);
        }
        std::cout << "[trace] operator internal sites armed for rva=0x"
                  << std::hex << rva << std::dec << "\n";
    }
}

static void logOperatorSite(HANDLE process, DWORD tid, const CONTEXT& context,
                            const Breakpoint& breakpoint, uintptr_t base,
                            std::vector<Breakpoint>& operatorTargets,
                            const std::vector<Breakpoint>& operatorSites) {
    const uintptr_t rva = breakpoint.address - base;
    uintptr_t table = 0;
    uintptr_t index = context.Rax;
    uintptr_t target = 0;
    const char* kind = "unknown";

    if (rva == 0x264911) {
        target = context.R15;
        kind = "call-r15";
    } else if (rva == 0x264941) {
        table = context.Rax;
        index = context.Rsi;
        kind = "call-table";
    } else if (rva == 0x2649c6) {
        target = context.Rax;
        kind = "tail-push-ret";
    } else if (rva == 0x2649e6) {
        table = context.R9;
        index = context.Rax;
        kind = "call-tail-table";
    }

    if (target == 0 && table != 0 && index < 0x100000) {
        readMemory(process, table + index * sizeof(uintptr_t), &target, sizeof(target));
    }

    std::cout << "[trace] operator-site rva=0x" << std::hex << rva
              << " kind=" << kind
              << " tid=" << tid
              << " table=0x" << table
              << " index=0x" << index
              << " target=0x" << target;
    if (target >= base) std::cout << " target-rva=0x" << (target - base);
    std::cout << std::dec << "\n";

    if (target == 0) return;
    std::cout << "[trace] operator target bytes: ";
    printBytes(process, target, 64);
    const uintptr_t targetRva = target >= base ? target - base : 0;
    const std::wstring dumpRoot = env(L"SG_OP_TARGET_DUMP");
    if (!dumpRoot.empty() && targetRva != 0 && target >= base && target < base + 0x800000) {
        std::wostringstream suffix;
        suffix << L".rva" << std::hex << targetRva;
        const std::wstring dumpPath = dumpRoot + suffix.str();
        std::vector<unsigned char> code(0x10000);
        if (readMemory(process, target, code.data(), code.size())) {
            restoreBreakpointsInDump(target, code, operatorTargets);
            restoreBreakpointsInDump(target, code, operatorSites);
            std::ofstream dump(std::filesystem::path(dumpPath), std::ios::binary);
            dump.write(reinterpret_cast<const char*>(code.data()),
                       static_cast<std::streamsize>(code.size()));
            std::cout << "[trace] operator target dump="
                      << std::string(dumpPath.begin(), dumpPath.end())
                      << " bytes=" << code.size() << "\n";
        }
    }
}

static void logIndirectSite(HANDLE process, DWORD tid, const CONTEXT& context,
                            uintptr_t base, const Breakpoint& breakpoint,
                            std::vector<Breakpoint>& operatorTargets,
                            const std::vector<Breakpoint>& operatorSites) {
    const uintptr_t rva = breakpoint.address - base;
    uintptr_t table = 0;
    uintptr_t target = 0;
    if (rva == 0x2615d7) {
        target = context.R11;
    } else if (rva == 0x1bb08e) {
        target = context.R9;
    } else if (rva == 0x1bb161) {
        target = context.Rdi;
    } else if (rva == 0x1bb1eb) {
        readMemory(process, context.Rbx + 0x90, &target, sizeof(target));
    } else if (rva == 0x1bb24e) {
        readMemory(process, context.Rbx + 0xa0, &target, sizeof(target));
    } else if (rva == 0x112bb8 || rva == 0x112beb || rva == 0x112fda) {
        table = context.R12;
    } else if (rva == 0x1bbf26 || rva == 0x1bbf45) {
        table = context.Rbx;
    } else if (rva == 0x113167) {
        table = context.Rdi;
    } else if (rva == 0x113222) {
        table = context.Rdx;
    } else if (rva == 0x1bc032 || rva == 0x1bc054) {
        table = context.R14;
    }
    uintptr_t index = context.Rax;
    if (rva == 0x1bc054) index = context.Rbx;
    if (target == 0 && table != 0) {
        readMemory(process, table + (index * sizeof(uintptr_t)),
                   &target, sizeof(target));
    }
    std::cout << "[trace] indirect-site rva=0x" << std::hex << rva
              << " tid=" << tid
              << " table=0x" << table
              << " index=0x" << index
              << " target=0x" << target;
    if (g_securityGuardBase != 0 && target >= g_securityGuardBase) {
        std::cout << " target-rva=0x" << (target - g_securityGuardBase);
    }
    std::cout << std::dec << "\n";
    if (target != 0) {
        std::cout << "[trace] indirect target bytes: ";
        printBytes(process, target, 32);
        const uintptr_t targetRva = g_securityGuardBase != 0 && target >= g_securityGuardBase
                                        ? target - g_securityGuardBase : 0;
        if (targetRva == 0x111c20) {
            const std::wstring dumpPath = env(L"SG_INDIRECT_DUMP");
            if (!dumpPath.empty()) {
                std::vector<unsigned char> code(0x10000);
                if (readMemory(process, target, code.data(), code.size())) {
                    std::ofstream dump(std::filesystem::path(dumpPath), std::ios::binary);
                    dump.write(reinterpret_cast<const char*>(code.data()),
                               static_cast<std::streamsize>(code.size()));
                    std::cout << "[trace] indirect target dump="
                              << std::string(dumpPath.begin(), dumpPath.end())
                              << " bytes=" << code.size() << "\n";
                }
            }
        }
        if (targetRva == 0x1bb010) {
            const std::wstring dumpPath = env(L"SG_TRANSFORM_DUMP");
            if (!dumpPath.empty()) {
                std::vector<unsigned char> code(0x10000);
                if (readMemory(process, target, code.data(), code.size())) {
                    std::ofstream dump(std::filesystem::path(dumpPath), std::ios::binary);
                    dump.write(reinterpret_cast<const char*>(code.data()),
                               static_cast<std::streamsize>(code.size()));
                    std::cout << "[trace] transform code dump="
                              << std::string(dumpPath.begin(), dumpPath.end())
                              << " bytes=" << code.size() << "\n";
                }
            }
        }
        if (targetRva == 0x2648a0 || targetRva == 0x264960) {
            std::wstring dumpPath = env(L"SG_OP_DUMP");
            if (!dumpPath.empty()) {
                dumpPath += targetRva == 0x2648a0 ? L".2648a0" : L".264960";
                std::vector<unsigned char> code(0x10000);
                if (readMemory(process, target, code.data(), code.size())) {
                    restoreBreakpointsInDump(target, code, operatorTargets);
                    restoreBreakpointsInDump(target, code, operatorSites);
                    std::ofstream dump(std::filesystem::path(dumpPath), std::ios::binary);
                    dump.write(reinterpret_cast<const char*>(code.data()),
                               static_cast<std::streamsize>(code.size()));
                    std::cout << "[trace] operator code dump="
                              << std::string(dumpPath.begin(), dumpPath.end())
                              << " bytes=" << code.size() << "\n";
                }
            }
            if (targetRva == 0x2648a0 || targetRva == 0x264960) {
                ensureBreakpoint(process, operatorTargets, target);
            }
        }
        if (rva == 0x2615d7) {
            const std::wstring dumpRoot = env(L"SG_OP_TARGET_DUMP");
            if (!dumpRoot.empty() && targetRva != 0 && target >= g_securityGuardBase &&
                target < g_securityGuardBase + 0x800000) {
                std::wostringstream suffix;
                suffix << L".site2615d7.rva" << std::hex << targetRva;
                const std::wstring dumpPath = dumpRoot + suffix.str();
                std::vector<unsigned char> code(0x10000);
                if (readMemory(process, target, code.data(), code.size())) {
                    restoreBreakpointsInDump(target, code, operatorTargets);
                    restoreBreakpointsInDump(target, code, operatorSites);
                    std::ofstream dump(std::filesystem::path(dumpPath), std::ios::binary);
                    dump.write(reinterpret_cast<const char*>(code.data()),
                               static_cast<std::streamsize>(code.size()));
                    std::cout << "[trace] indirect target dump="
                              << std::string(dumpPath.begin(), dumpPath.end())
                              << " bytes=" << code.size() << "\n";
                }
            }
        }
    }
}

int wmain() {
    const std::wstring node = env(L"SG_NODE");
    const std::wstring script = env(L"SG_WAIT_SCRIPT");
    if (node.empty() || script.empty()) {
        std::wcerr << L"SG_NODE and SG_WAIT_SCRIPT are required\n";
        return 2;
    }

    std::wstring command = quote(node) + L" " + quote(script);
    std::vector<wchar_t> commandLine(command.begin(), command.end());
    commandLine.push_back(L'\0');

    STARTUPINFOW startup{};
    startup.cb = sizeof(startup);
    PROCESS_INFORMATION processInfo{};
    const DWORD flags = CREATE_NO_WINDOW | DEBUG_ONLY_THIS_PROCESS;
    if (!CreateProcessW(node.c_str(), commandLine.data(), nullptr, nullptr, FALSE, flags,
                        nullptr, nullptr, &startup, &processInfo)) {
        std::wcerr << L"CreateProcessW failed: " << GetLastError() << L"\n";
        return 1;
    }

    Breakpoint method;
    Breakpoint inner;
    Breakpoint parseStart;
    Breakpoint fieldLookup;
    Breakpoint fieldReturn;
    Breakpoint signExtract;
    std::vector<Breakpoint> helperBreakpoints;
    std::vector<Breakpoint> indirectSites;
    Breakpoint callEntry;
    Breakpoint callReturn;
    Breakpoint tableInitCall;
    Breakpoint tableFieldStore;
    Breakpoint sboxByteStore;
    Breakpoint sboxRandState;
    Breakpoint sboxRandSeed;
    Breakpoint sboxSeedSourceReturn;
    Breakpoint generatorReturn;
    Breakpoint tableBuildReturn;
    Breakpoint initCallback;
    Breakpoint initDispatch;
    Breakpoint initCompletion;
    Breakpoint sdkInitUmid;
    Breakpoint sdkInitReturn;
    Breakpoint sdkUmidCollection;
    Breakpoint sdkUmidPrepare;
    Breakpoint sdkUmidFinalize;
    Breakpoint sdkGetSecurityToken;
    SingleStep step;
    bool observed = false;
    bool done = false;
    bool generatorReturnOverlapsSignExtract = false;
    std::vector<Breakpoint> operatorTargets;
    std::vector<Breakpoint> operatorSites;
    std::vector<Breakpoint> initTargets;
    std::vector<Breakpoint> initNativeCallSites;
    std::vector<Breakpoint> initNativeTargets;
    std::vector<Breakpoint> dispatchSites;
    unsigned int stopAfter = 1;
    const std::wstring stopAfterText = env(L"SG_STOP_AFTER");
    if (!stopAfterText.empty()) {
        try {
            stopAfter = std::max<unsigned int>(1, static_cast<unsigned int>(std::stoul(stopAfterText)));
        } catch (...) {
            stopAfter = 1;
        }
    }
    unsigned int observedCalls = 0;
    const bool traceInit = env(L"SG_TRACE_INIT") == L"1";
    const bool initOnly = env(L"SG_INIT_ONLY") == L"1";
    const bool traceInitReturn = env(L"SG_TRACE_INIT_RETURN") == L"1";
    const bool traceInitDeep = env(L"SG_TRACE_INIT_DEEP") == L"1";
    const bool traceInitFinalize = env(L"SG_TRACE_INIT_FINALIZE") == L"1";

    while (!done) {
        DEBUG_EVENT event{};
        if (!WaitForDebugEvent(&event, 30000)) {
            std::cerr << "WaitForDebugEvent failed/timeout: " << GetLastError() << "\n";
            break;
        }

        DWORD continueStatus = DBG_CONTINUE;
        switch (event.dwDebugEventCode) {
        case CREATE_PROCESS_DEBUG_EVENT:
            if (event.u.CreateProcessInfo.hFile != nullptr) {
                CloseHandle(event.u.CreateProcessInfo.hFile);
            }
            break;

        case LOAD_DLL_DEBUG_EVENT: {
            const auto base = event.u.LoadDll.lpBaseOfDll;
            std::wstring name = fileNameFromHandle(event.u.LoadDll.hFile);
            if (name.empty()) name = moduleName(processInfo.hProcess, base);
            std::wcout << L"[trace] load " << name << L" base=" << base << L"\n";
            if (isD0TraceModule(name)) {
                ModuleRange range;
                range.base = reinterpret_cast<uintptr_t>(base);
                range.size = remoteImageSize(processInfo.hProcess, range.base);
                range.name = name;
                if (range.size != 0) g_loadedD0Modules.push_back(std::move(range));
                armD0WriteBreakpoints(processInfo.hProcess, g_loadedD0Modules);
            }
            if (endsWithInsensitive(name, L"AliSafeProxy.dll")) {
                method.address = reinterpret_cast<uintptr_t>(base) + 0x95790;
                if (env(L"SG_TRACE_METHOD") == L"1") {
                    if (!arm(processInfo.hProcess, method)) {
                        std::cerr << "failed to arm AliSafeProxy breakpoint\n";
                    } else {
                        std::cout << "[trace] method breakpoint armed at 0x" << std::hex
                                  << method.address << std::dec << "\n";
                    }
                }
            }
            if (endsWithInsensitive(name, L"SecurityGuardSDK64.dll")) {
                g_securityGuardBase = reinterpret_cast<uintptr_t>(base);
                std::cout << "[trace] SecurityGuardSDK64 base=0x" << std::hex
                          << g_securityGuardBase << std::dec << "\n";
                if (traceInit) {
                    sdkInitUmid.address = g_securityGuardBase + 0xfd1e0;
                    if (!arm(processInfo.hProcess, sdkInitUmid)) {
                        std::cerr << "failed to arm SDK initUMID breakpoint\n";
                    } else {
                        std::cout << "[trace] SDK initUMID breakpoint armed at 0x"
                                  << std::hex << sdkInitUmid.address << std::dec << "\n";
                    }
                    sdkGetSecurityToken.address = g_securityGuardBase + 0xfd570;
                    if (!arm(processInfo.hProcess, sdkGetSecurityToken)) {
                        std::cerr << "failed to arm SDK getSecurityToken breakpoint\n";
                    } else {
                        std::cout << "[trace] SDK getSecurityToken breakpoint armed at 0x"
                                  << std::hex << sdkGetSecurityToken.address << std::dec << "\n";
                    }
                    if (traceInitDeep) {
                        auto armDeep = [&](Breakpoint& breakpoint, uintptr_t rva,
                                           const char* label) {
                            breakpoint.address = g_securityGuardBase + rva;
                            if (!arm(processInfo.hProcess, breakpoint)) {
                                std::cerr << "failed to arm " << label << " breakpoint\n";
                            } else {
                                std::cout << "[trace] " << label << " breakpoint armed at 0x"
                                          << std::hex << breakpoint.address << std::dec << "\n";
                            }
                        };
                        armDeep(sdkUmidCollection, 0x60ae0, "SDK UMID collection");
                        armDeep(sdkUmidPrepare, 0x108780, "SDK UMID prepare");
                        if (traceInitFinalize) {
                            armDeep(sdkUmidFinalize, 0x333310, "SDK UMID finalize");
                        }
                    }
                }
            }
            if (endsWithInsensitive(name, L"security_guard.node")) {
                if (traceInit) {
                    initCallback.address = reinterpret_cast<uintptr_t>(base) + 0x42c0;
                    if (!arm(processInfo.hProcess, initCallback)) {
                        std::cerr << "failed to arm Node initUmid breakpoint\n";
                    } else {
                        std::cout << "[trace] Node initUmid breakpoint armed at 0x"
                                  << std::hex << initCallback.address << std::dec << "\n";
                    }
                    initDispatch.address = reinterpret_cast<uintptr_t>(base) + 0x486d;
                    if (!arm(processInfo.hProcess, initDispatch)) {
                        std::cerr << "failed to arm IUmid::InitUMID dispatch breakpoint\n";
                    } else {
                        std::cout << "[trace] IUmid::InitUMID dispatch breakpoint armed at 0x"
                                  << std::hex << initDispatch.address << std::dec << "\n";
                    }
                    initCompletion.address = reinterpret_cast<uintptr_t>(base) + 0x5f30;
                    if (!arm(processInfo.hProcess, initCompletion)) {
                        std::cerr << "failed to arm UMID completion callback breakpoint\n";
                    } else {
                        std::cout << "[trace] UMID completion callback breakpoint armed at 0x"
                                  << std::hex << initCompletion.address << std::dec << "\n";
                    }
                }
                callEntry.address = reinterpret_cast<uintptr_t>(base) + 0x3927;
                callReturn.address = reinterpret_cast<uintptr_t>(base) + 0x392a;
                if (!arm(processInfo.hProcess, callEntry)) {
                    std::cerr << "failed to arm Node call-entry breakpoint\n";
                } else {
                    std::cout << "[trace] call-entry breakpoint armed at 0x" << std::hex
                              << callEntry.address << std::dec << "\n";
                }
                if (!arm(processInfo.hProcess, callReturn)) {
                    std::cerr << "failed to arm Node call-return breakpoint\n";
                } else {
                    std::cout << "[trace] call-return breakpoint armed at 0x" << std::hex
                              << callReturn.address << std::dec << "\n";
                }
            }
            if (event.u.LoadDll.hFile != nullptr) {
                CloseHandle(event.u.LoadDll.hFile);
            }
            break;
        }

        case EXCEPTION_DEBUG_EVENT: {
            const auto& exception = event.u.Exception.ExceptionRecord;
            if (exception.ExceptionCode == EXCEPTION_BREAKPOINT) {
                CONTEXT context{};
                if (!contextFor(event.dwThreadId, context)) break;
                const uintptr_t breakpointAddress = context.Rip - 1;
                Breakpoint* hit = nullptr;
                if (method.installed && breakpointAddress == method.address) hit = &method;
                if (inner.installed && breakpointAddress == inner.address) hit = &inner;
                if (initCallback.installed && breakpointAddress == initCallback.address) {
                    hit = &initCallback;
                }
                if (initDispatch.installed && breakpointAddress == initDispatch.address) {
                    hit = &initDispatch;
                }
                if (initCompletion.installed && breakpointAddress == initCompletion.address) {
                    hit = &initCompletion;
                }
                if (sdkInitUmid.installed && breakpointAddress == sdkInitUmid.address) {
                    hit = &sdkInitUmid;
                }
                if (traceInitReturn && sdkInitReturn.installed &&
                    breakpointAddress == sdkInitReturn.address) {
                    hit = &sdkInitReturn;
                }
                if (sdkUmidCollection.installed &&
                    breakpointAddress == sdkUmidCollection.address) {
                    hit = &sdkUmidCollection;
                }
                if (sdkUmidPrepare.installed &&
                    breakpointAddress == sdkUmidPrepare.address) {
                    hit = &sdkUmidPrepare;
                }
                if (sdkUmidFinalize.installed &&
                    breakpointAddress == sdkUmidFinalize.address) {
                    hit = &sdkUmidFinalize;
                }
                if (sdkGetSecurityToken.installed &&
                    breakpointAddress == sdkGetSecurityToken.address) {
                    hit = &sdkGetSecurityToken;
                }
                if (parseStart.installed && breakpointAddress == parseStart.address) hit = &parseStart;
                if (fieldLookup.installed && breakpointAddress == fieldLookup.address) hit = &fieldLookup;
                if (fieldReturn.installed && breakpointAddress == fieldReturn.address) hit = &fieldReturn;
                if (signExtract.installed && breakpointAddress == signExtract.address) hit = &signExtract;
                if (callEntry.installed && breakpointAddress == callEntry.address) hit = &callEntry;
                if (callReturn.installed && breakpointAddress == callReturn.address) hit = &callReturn;
                if (generatorReturn.installed && breakpointAddress == generatorReturn.address) {
                    hit = &generatorReturn;
                }
                if (tableBuildReturn.installed && breakpointAddress == tableBuildReturn.address) {
                    hit = &tableBuildReturn;
                }
                if (hit == nullptr) {
                    if (sboxSeedSourceReturn.installed &&
                        breakpointAddress == sboxSeedSourceReturn.address) {
                        hit = &sboxSeedSourceReturn;
                    }
                }
                if (hit == nullptr) {
                    if (sboxRandSeed.installed && breakpointAddress == sboxRandSeed.address) {
                        hit = &sboxRandSeed;
                    }
                }
                if (hit == nullptr) {
                    if (sboxRandState.installed && breakpointAddress == sboxRandState.address) {
                        hit = &sboxRandState;
                    }
                }
                if (hit == nullptr) {
                    if (sboxByteStore.installed && breakpointAddress == sboxByteStore.address) {
                        hit = &sboxByteStore;
                    }
                }
                if (hit == nullptr) {
                    if (tableFieldStore.installed && breakpointAddress == tableFieldStore.address) {
                        hit = &tableFieldStore;
                    }
                }
                if (hit == nullptr) {
                    for (auto& breakpoint : g_d0WriteBreakpoints) {
                        if (breakpoint.installed && breakpointAddress == breakpoint.address) {
                            hit = &breakpoint;
                            break;
                        }
                    }
                }
                if (hit == nullptr) {
                    for (auto& site : dispatchSites) {
                        if (site.installed && breakpointAddress == site.address) {
                            hit = &site;
                            break;
                        }
                    }
                }
                if (hit == nullptr) {
                    for (auto& helper : helperBreakpoints) {
                        if (helper.installed && breakpointAddress == helper.address) {
                            hit = &helper;
                            break;
                        }
                    }
                }
                if (hit == nullptr) {
                    for (auto& breakpoint : operatorTargets) {
                        if (breakpoint.installed && breakpointAddress == breakpoint.address) {
                            hit = &breakpoint;
                            break;
                        }
                    }
                }
                if (hit == nullptr) {
                    for (auto& breakpoint : operatorSites) {
                        if (breakpoint.installed && breakpointAddress == breakpoint.address) {
                            hit = &breakpoint;
                            break;
                        }
                    }
                }
                if (hit == nullptr) {
                    for (auto& breakpoint : initTargets) {
                        if (breakpoint.installed && breakpointAddress == breakpoint.address) {
                            hit = &breakpoint;
                            break;
                        }
                    }
                }
                if (hit == nullptr) {
                    for (auto& breakpoint : initNativeCallSites) {
                        if (breakpoint.installed && breakpointAddress == breakpoint.address) {
                            hit = &breakpoint;
                            break;
                        }
                    }
                }
                if (hit == nullptr) {
                    for (auto& breakpoint : initNativeTargets) {
                        if (breakpoint.installed && breakpointAddress == breakpoint.address) {
                            hit = &breakpoint;
                            break;
                        }
                    }
                }
                if (hit == nullptr) {
                    for (auto& site : indirectSites) {
                        if (site.installed && breakpointAddress == site.address) {
                            hit = &site;
                            break;
                        }
                    }
                }
                if (tableInitCall.installed && breakpointAddress == tableInitCall.address) {
                    hit = &tableInitCall;
                }
                if (hit == nullptr) break;

                if (hit == &method) {
                    logMethodEntry(processInfo.hProcess, event.dwThreadId, context);
                } else if (hit == &initCallback) {
                    logInitEntry(processInfo.hProcess, event.dwThreadId, context,
                                 initCallback.address, initCallback, "init-callback");
                } else if (hit == &initDispatch) {
                    logInitDispatch(processInfo.hProcess, event.dwThreadId, context,
                                    initCallback.address - 0x42c0, initDispatch,
                                    initTargets);
                } else if (hit == &initCompletion) {
                    logInitEntry(processInfo.hProcess, event.dwThreadId, context,
                                 initCompletion.address - 0x5f30, initCompletion,
                                 "umid-completion");
                } else if (hit == &sdkInitUmid) {
                    logInitEntry(processInfo.hProcess, event.dwThreadId, context,
                                 g_securityGuardBase, sdkInitUmid, "sdk-initUMID");
                    armD0WriteBreakpoints(processInfo.hProcess,
                                          traceD0Modules(processInfo.hProcess));
                    if (traceInitDeep) {
                        const bool collectionArmed = ensurePhysicalBreakpoint(
                            processInfo.hProcess, sdkUmidCollection);
                        const bool prepareArmed = ensurePhysicalBreakpoint(
                            processInfo.hProcess, sdkUmidPrepare);
                        const bool finalizeArmed = !traceInitFinalize ||
                            ensurePhysicalBreakpoint(processInfo.hProcess, sdkUmidFinalize);
                        std::cout << "[trace] SDK UMID deep rearm collection="
                                  << (collectionArmed ? "ok" : "failed")
                                  << " prepare=" << (prepareArmed ? "ok" : "failed")
                                  << " finalize=" << (finalizeArmed ? "ok" : "failed")
                                  << "\n";
                    }
                    if (traceInitReturn) {
                        if (sdkInitReturn.installed) disarm(processInfo.hProcess, sdkInitReturn);
                        uintptr_t returnAddress = 0;
                        if (readMemory(processInfo.hProcess, context.Rsp, &returnAddress,
                                       sizeof(returnAddress))) {
                            sdkInitReturn.address = returnAddress;
                            const bool armed = arm(processInfo.hProcess, sdkInitReturn);
                            std::cout << "[trace] SDK initUMID return breakpoint="
                                      << (armed ? "armed" : "failed") << " at 0x"
                                      << std::hex << returnAddress << std::dec << "\n";
                        }
                    }
                } else if (hit == &sdkInitReturn) {
                    logInitEntry(processInfo.hProcess, event.dwThreadId, context,
                                  g_securityGuardBase, sdkInitReturn, "sdk-initUMID-return");
                } else if (hit == &sdkUmidCollection) {
                    logInitEntry(processInfo.hProcess, event.dwThreadId, context,
                                 g_securityGuardBase, sdkUmidCollection,
                                 "sdk-UMID-collection");
                } else if (hit == &sdkUmidPrepare) {
                    logInitEntry(processInfo.hProcess, event.dwThreadId, context,
                                 g_securityGuardBase, sdkUmidPrepare,
                                 "sdk-UMID-prepare");
                } else if (hit == &sdkUmidFinalize) {
                    logInitEntry(processInfo.hProcess, event.dwThreadId, context,
                                 g_securityGuardBase, sdkUmidFinalize,
                                 "sdk-UMID-finalize");
                } else if (hit == &sdkGetSecurityToken) {
                    logInitEntry(processInfo.hProcess, event.dwThreadId, context,
                                 g_securityGuardBase, sdkGetSecurityToken,
                                 "sdk-getSecurityToken");
                } else if (hit == &inner) {
                    logInnerEntry(processInfo.hProcess, event.dwThreadId, context, inner);
                    if (initOnly) {
                        // Keep the process alive for the normal call-return stop,
                        // but avoid flooding the focused init capture with factor helpers.
                    } else {
                    parseStart.address = g_innerBase + 0xe17e9;
                    if (!arm(processInfo.hProcess, parseStart)) {
                        std::cerr << "failed to arm parse breakpoint\n";
                    } else {
                        std::cout << "[trace] parse breakpoint armed at 0x" << std::hex
                                  << parseStart.address << std::dec << "\n";
                    }
                    fieldLookup.address = g_innerBase + 0x305f0;
                    if (!arm(processInfo.hProcess, fieldLookup)) {
                        std::cerr << "failed to arm field lookup breakpoint\n";
                    } else {
                        std::cout << "[trace] field lookup breakpoint armed at 0x" << std::hex
                                  << fieldLookup.address << std::dec << "\n";
                    }
                    for (uintptr_t rva : {
                             0x2eec0, 0x2e0a0, 0x2e7c0, 0x41720,
                             0x5d04f0, 0x14ab0, 0x5b1140, 0xe04e0,
                             0x178a0, 0x5b0fc0, 0x5b1040, 0x7a430,
                             0xb57e0, 0x19620, 0x8fc10, 0xfd570,
                             0xdf480, 0x2db90, 0x112a10, 0x416a0,
                              0x33440, 0x33270, 0x30ae0, 0xf9440,
                              0xed330, 0x89a10, 0x2ac90, 0x33c0,
                              0x111c20, 0x1bb4e0, 0x1bb5f0, 0x1bbe70,
                               0x1bbb20, 0x1bbdc0, 0x1bb010,
                              0x261040, 0x2651e0, 0x261372, 0x2654e0}) {
                        if (rva == 0x305f0) continue;
                        Breakpoint helper;
                        helper.address = g_innerBase + rva;
                        if (arm(processInfo.hProcess, helper)) {
                            helperBreakpoints.push_back(helper);
                        }
                    }
                    std::cout << "[trace] helper breakpoints armed="
                              << helperBreakpoints.size() << "\n";
                    signExtract.address = g_innerBase + 0xe2b46;
                    if (!arm(processInfo.hProcess, signExtract)) {
                        std::cerr << "failed to arm sign extract breakpoint\n";
                    } else {
                        std::cout << "[trace] sign extract breakpoint armed at 0x" << std::hex
                                  << signExtract.address << std::dec << "\n";
                    }
                    for (uintptr_t rva : {0x112bb8, 0x112beb, 0x112fda,
                                          0x1130fb, 0x113167, 0x113222,
                                          0x1bbf26, 0x1bbf45, 0x1bc032,
                                          0x1bc054, 0x1bb08e, 0x1bb161,
                                          0x1bb1eb, 0x1bb24e, 0x2615d7}) {
                        Breakpoint site;
                        site.address = g_innerBase + rva;
                        if (arm(processInfo.hProcess, site)) {
                            indirectSites.push_back(site);
                        }
                     }
                     std::cout << "[trace] indirect sites armed="
                               << indirectSites.size() << "\n";
                     for (uintptr_t rva : {0x1bb55b, 0x1bb5c8,
                                           0x1bb69a, 0x1bb6b3}) {
                         Breakpoint site;
                         site.address = g_innerBase + rva;
                         if (arm(processInfo.hProcess, site)) {
                             dispatchSites.push_back(site);
                         }
                     }
                     std::cout << "[trace] dispatch sites armed="
                               << dispatchSites.size() << "\n";
                     if (env(L"SG_TRACE_SBOX_BYTES") == L"1") {
                         for (uintptr_t rva : {0x28e13a, 0x28e88d, 0x28e89e}) {
                             Breakpoint site;
                             site.address = g_innerBase + rva;
                             if (arm(processInfo.hProcess, site)) {
                                 dispatchSites.push_back(site);
                             }
                             std::cout << "[trace] sbox dispatch site rva=0x"
                                       << std::hex << rva << " "
                                       << (site.installed ? "armed" : "failed")
                                       << std::dec << "\n";
                         }
                     }
                     tableInitCall.address = g_innerBase + 0x111c66;
                     if (!arm(processInfo.hProcess, tableInitCall)) {
                         std::cerr << "failed to arm table-init call breakpoint\n";
                     } else {
                         std::cout << "[trace] table-init call breakpoint armed at 0x"
                                   << std::hex << tableInitCall.address << std::dec << "\n";
                     }
                     armD0WriteBreakpoints(processInfo.hProcess,
                                           traceD0Modules(processInfo.hProcess));
                    }
                } else if (hit == &parseStart) {
                    logParseStart(processInfo.hProcess, event.dwThreadId, context);
                } else if (hit == &fieldLookup) {
                    logFieldLookup(processInfo.hProcess, event.dwThreadId, context);
                    if (fieldReturn.installed) disarm(processInfo.hProcess, fieldReturn);
                    uintptr_t returnAddress = 0;
                    if (readMemory(processInfo.hProcess, context.Rsp, &returnAddress,
                                   sizeof(returnAddress))) {
                        fieldReturn.address = returnAddress;
                        if (!arm(processInfo.hProcess, fieldReturn)) {
                            std::cerr << "failed to arm field return breakpoint\n";
                        }
                    }
                } else if (hit == &fieldReturn) {
                    logFieldReturn(processInfo.hProcess, event.dwThreadId, context);
                    arm(processInfo.hProcess, fieldLookup);
                } else if (hit == &signExtract) {
                    if (generatorReturnOverlapsSignExtract) {
                        logGeneratorReturn(processInfo.hProcess, event.dwThreadId, context,
                                           signExtract);
                        generatorReturnOverlapsSignExtract = false;
                    }
                    logSignExtract(processInfo.hProcess, event.dwThreadId, context);
                } else if (std::find_if(operatorTargets.begin(), operatorTargets.end(),
                                        [hit](const Breakpoint& breakpoint) {
                                            return &breakpoint == hit;
                                        }) != operatorTargets.end()) {
                    logOperatorEntry(processInfo.hProcess, event.dwThreadId, context,
                                     *hit, g_securityGuardBase, operatorSites);
                } else if (std::find_if(operatorSites.begin(), operatorSites.end(),
                                        [hit](const Breakpoint& breakpoint) {
                                            return &breakpoint == hit;
                                        }) != operatorSites.end()) {
                    logOperatorSite(processInfo.hProcess, event.dwThreadId, context,
                                     *hit, g_securityGuardBase, operatorTargets,
                                     operatorSites);
                } else if (std::find_if(initTargets.begin(), initTargets.end(),
                                        [hit](const Breakpoint& breakpoint) {
                                            return &breakpoint == hit;
                                        }) != initTargets.end()) {
                    logInitEntry(processInfo.hProcess, event.dwThreadId, context,
                                 g_securityGuardBase, *hit, "init-target");
                    ensureBreakpoint(processInfo.hProcess, initNativeCallSites,
                                     hit->address + 0x125);
                } else if (std::find_if(initNativeCallSites.begin(), initNativeCallSites.end(),
                                        [hit](const Breakpoint& breakpoint) {
                                            return &breakpoint == hit;
                                        }) != initNativeCallSites.end()) {
                    logInitNativeCall(processInfo.hProcess, event.dwThreadId, context,
                                      *hit, initNativeTargets, sdkInitUmid);
                } else if (std::find_if(initNativeTargets.begin(), initNativeTargets.end(),
                                        [hit](const Breakpoint& breakpoint) {
                                            return &breakpoint == hit;
                                        }) != initNativeTargets.end()) {
                    logInitEntry(processInfo.hProcess, event.dwThreadId, context,
                                 g_securityGuardBase, *hit, "init-native-target");
                } else if (std::find_if(indirectSites.begin(), indirectSites.end(),
                                        [hit](const Breakpoint& site) {
                                            return &site == hit;
                                        }) != indirectSites.end()) {
                    logIndirectSite(processInfo.hProcess, event.dwThreadId, context,
                                    g_innerBase, *hit, operatorTargets, operatorSites);
                 } else if (hit == &callEntry) {
                     logCallEntry(processInfo.hProcess, event.dwThreadId, context);
                     armD0WriteBreakpoints(processInfo.hProcess,
                                           traceD0Modules(processInfo.hProcess));
                     if (env(L"SG_TRACE_TABLE_FIELD") == L"1" && !tableFieldStore.installed) {
                         tableFieldStore.address = g_securityGuardBase + 0x28d61a;
                         if (!arm(processInfo.hProcess, tableFieldStore)) {
                             std::cerr << "failed to arm table-field store breakpoint\n";
                         } else {
                             std::cout << "[trace] table-field store breakpoint armed at 0x"
                                       << std::hex << tableFieldStore.address << std::dec << "\n";
                         }
                     }
                     if (env(L"SG_TRACE_SBOX_BYTES") == L"1" && !sboxByteStore.installed) {
                         sboxByteStore.address = g_securityGuardBase + 0x28e156;
                         if (!arm(processInfo.hProcess, sboxByteStore)) {
                             std::cerr << "failed to arm sbox byte-store breakpoint\n";
                         } else {
                             std::cout << "[trace] sbox byte-store breakpoint armed at 0x"
                                       << std::hex << sboxByteStore.address << std::dec << "\n";
                         }
                     }
                     if (env(L"SG_TRACE_SBOX_BYTES") == L"1" && !sboxRandState.installed) {
                         sboxRandState.address = g_securityGuardBase + 0x5cf43e;
                         if (!arm(processInfo.hProcess, sboxRandState)) {
                             std::cerr << "failed to arm sbox rand breakpoint\n";
                         } else {
                             std::cout << "[trace] sbox rand breakpoint armed at 0x"
                                       << std::hex << sboxRandState.address << std::dec << "\n";
                         }
                     }
                     if (env(L"SG_TRACE_SBOX_BYTES") == L"1" && !sboxRandSeed.installed) {
                         sboxRandSeed.address = g_securityGuardBase + 0x5cf41d;
                         if (!arm(processInfo.hProcess, sboxRandSeed)) {
                             std::cerr << "failed to arm sbox seed breakpoint\n";
                         } else {
                             std::cout << "[trace] sbox seed breakpoint armed at 0x"
                                       << std::hex << sboxRandSeed.address << std::dec << "\n";
                         }
                     }
                     if (env(L"SG_TRACE_SBOX_BYTES") == L"1" &&
                         !sboxSeedSourceReturn.installed) {
                         sboxSeedSourceReturn.address = g_securityGuardBase + 0x28e890;
                         if (!arm(processInfo.hProcess, sboxSeedSourceReturn)) {
                             std::cerr << "failed to arm sbox seed-source return breakpoint\n";
                         } else {
                             std::cout << "[trace] sbox seed-source return breakpoint armed at 0x"
                                       << std::hex << sboxSeedSourceReturn.address << std::dec << "\n";
                         }
                     }
                     uintptr_t innerObject = 0;
                    uintptr_t innerVtable = 0;
                    uintptr_t innerFactors = 0;
                    if (readMemory(processInfo.hProcess, context.Rcx + 8, &innerObject, sizeof(innerObject)) &&
                        readMemory(processInfo.hProcess, innerObject, &innerVtable, sizeof(innerVtable)) &&
                        readMemory(processInfo.hProcess, innerVtable + 8 * sizeof(uintptr_t),
                                   &innerFactors, sizeof(innerFactors))) {
                        inner.address = innerFactors;
                        if (!arm(processInfo.hProcess, inner)) {
                            std::cerr << "failed to arm inner SDK breakpoint\n";
                        } else {
                            std::cout << "[trace] inner SDK breakpoint armed at 0x" << std::hex
                                      << inner.address << std::dec << "\n";
                        }
                     }
                 } else if (hit == &tableInitCall) {
                     logTableInitCall(processInfo.hProcess, event.dwThreadId, context);
                 } else if (std::find_if(helperBreakpoints.begin(), helperBreakpoints.end(),
                                         [hit](const Breakpoint& helper) {
                                             return &helper == hit;
                                         }) != helperBreakpoints.end()) {
                      logHelperEntry(processInfo.hProcess, event.dwThreadId, context,
                                     g_innerBase, *hit, generatorReturn,
                                     generatorReturnOverlapsSignExtract,
                                     tableBuildReturn);
                 } else if (hit == &sboxSeedSourceReturn) {
                     logSboxSeedSource(event.dwThreadId, context, sboxSeedSourceReturn);
                 } else if (hit == &sboxRandSeed) {
                     logSboxRandSeed(processInfo.hProcess, event.dwThreadId, context,
                                     sboxRandSeed);
                 } else if (hit == &sboxRandState) {
                     logSboxRandState(processInfo.hProcess, event.dwThreadId, context,
                                      sboxRandState);
                 } else if (hit == &sboxByteStore) {
                     logSboxByteStore(processInfo.hProcess, event.dwThreadId, context,
                                      sboxByteStore);
                 } else if (hit == &tableFieldStore) {
                     logTableFieldStore(processInfo.hProcess, event.dwThreadId, context);
                 } else if (std::find_if(g_d0WriteBreakpoints.begin(), g_d0WriteBreakpoints.end(),
                                          [hit](const Breakpoint& breakpoint) {
                                              return &breakpoint == hit;
                                          }) != g_d0WriteBreakpoints.end()) {
                     logD0Write(processInfo.hProcess, context, *hit,
                                d0ProbeModuleBase(hit->address));
                 } else if (std::find_if(dispatchSites.begin(), dispatchSites.end(),
                                         [hit](const Breakpoint& site) {
                                             return &site == hit;
                                         }) != dispatchSites.end()) {
                     logDispatchSite(processInfo.hProcess, g_innerBase, context, *hit);
                 } else if (hit == &generatorReturn) {
                     logGeneratorReturn(processInfo.hProcess, event.dwThreadId, context,
                                        generatorReturn);
                 } else if (hit == &tableBuildReturn) {
                    logTableBuildReturn(processInfo.hProcess, event.dwThreadId, context,
                                        tableBuildReturn);
                } else if (hit == &callReturn) {
                    logCallReturn(processInfo.hProcess, event.dwThreadId, context);
                    observed = ++observedCalls >= stopAfter;
                } else {
                    continueStatus = DBG_EXCEPTION_NOT_HANDLED;
                }

                disarm(processInfo.hProcess, *hit);
                context.Rip = hit->address;
                context.EFlags |= 0x100;
                setContextFor(event.dwThreadId, context);
                step.active = true;
                step.breakpoint = hit;
            } else if (exception.ExceptionCode == EXCEPTION_SINGLE_STEP) {
                CONTEXT context{};
                const bool haveContext = contextFor(event.dwThreadId, context);
                const bool hardwareWatchHit = haveContext &&
                    handleHardwareWatch(processInfo.hProcess, event.dwThreadId, context);
                if (hardwareWatchHit && step.active) {
                    context.EFlags &= ~0x100u;
                    setContextFor(event.dwThreadId, context);
                    arm(processInfo.hProcess, *step.breakpoint);
                    step.active = false;
                    step.breakpoint = nullptr;
                } else if (!hardwareWatchHit && step.active) {
                    context.EFlags &= ~0x100u;
                    setContextFor(event.dwThreadId, context);
                    arm(processInfo.hProcess, *step.breakpoint);
                    step.active = false;
                    step.breakpoint = nullptr;
                } else if (!hardwareWatchHit) {
                    std::cout << "[trace] external single-step tid=" << event.dwThreadId
                              << " rip=0x" << std::hex << context.Rip
                              << " dr6=0x";
                    HANDLE thread = OpenThread(THREAD_GET_CONTEXT, FALSE, event.dwThreadId);
                    CONTEXT debug{};
                    debug.ContextFlags = CONTEXT_DEBUG_REGISTERS;
                    if (thread != nullptr && GetThreadContext(thread, &debug)) {
                        std::cout << debug.Dr6;
                    } else {
                        std::cout << "<unreadable>";
                    }
                    if (thread != nullptr) CloseHandle(thread);
                    std::cout << std::dec << "\n";
                    context.EFlags &= ~0x100u;
                    setContextFor(event.dwThreadId, context);
                }
            } else {
                continueStatus = DBG_EXCEPTION_NOT_HANDLED;
            }
            break;
        }

        case EXIT_PROCESS_DEBUG_EVENT:
            std::cout << "[trace] child-exit code="
                      << event.u.ExitProcess.dwExitCode << "\n";
            done = true;
            break;

        default:
            break;
        }

        ContinueDebugEvent(event.dwProcessId, event.dwThreadId, continueStatus);
        if (observed) {
            TerminateProcess(processInfo.hProcess, 0);
            observed = false;
        }
    }

    if (method.installed) disarm(processInfo.hProcess, method);
    if (inner.installed) disarm(processInfo.hProcess, inner);
    if (initCallback.installed) disarm(processInfo.hProcess, initCallback);
    if (initDispatch.installed) disarm(processInfo.hProcess, initDispatch);
    if (initCompletion.installed) disarm(processInfo.hProcess, initCompletion);
    if (sdkInitUmid.installed) disarm(processInfo.hProcess, sdkInitUmid);
    if (sdkInitReturn.installed) disarm(processInfo.hProcess, sdkInitReturn);
    if (sdkUmidCollection.installed) disarm(processInfo.hProcess, sdkUmidCollection);
    if (sdkUmidPrepare.installed) disarm(processInfo.hProcess, sdkUmidPrepare);
    if (sdkUmidFinalize.installed) disarm(processInfo.hProcess, sdkUmidFinalize);
    if (sdkGetSecurityToken.installed) disarm(processInfo.hProcess, sdkGetSecurityToken);
    for (auto& target : initTargets) {
        if (target.installed) disarm(processInfo.hProcess, target);
    }
    for (auto& site : initNativeCallSites) {
        if (site.installed) disarm(processInfo.hProcess, site);
    }
    for (auto& target : initNativeTargets) {
        if (target.installed) disarm(processInfo.hProcess, target);
    }
    if (parseStart.installed) disarm(processInfo.hProcess, parseStart);
    if (fieldLookup.installed) disarm(processInfo.hProcess, fieldLookup);
    if (fieldReturn.installed) disarm(processInfo.hProcess, fieldReturn);
    if (signExtract.installed) disarm(processInfo.hProcess, signExtract);
    for (auto& site : indirectSites) {
        if (site.installed) disarm(processInfo.hProcess, site);
    }
    for (auto& site : operatorSites) {
        if (site.installed) disarm(processInfo.hProcess, site);
    }
    for (auto& target : operatorTargets) {
        if (target.installed) disarm(processInfo.hProcess, target);
    }
    for (auto& helper : helperBreakpoints) {
        if (helper.installed) disarm(processInfo.hProcess, helper);
    }
    if (callEntry.installed) disarm(processInfo.hProcess, callEntry);
    if (callReturn.installed) disarm(processInfo.hProcess, callReturn);
    if (tableInitCall.installed) disarm(processInfo.hProcess, tableInitCall);
    if (tableFieldStore.installed) disarm(processInfo.hProcess, tableFieldStore);
    if (sboxByteStore.installed) disarm(processInfo.hProcess, sboxByteStore);
    if (sboxRandState.installed) disarm(processInfo.hProcess, sboxRandState);
    if (sboxRandSeed.installed) disarm(processInfo.hProcess, sboxRandSeed);
     if (sboxSeedSourceReturn.installed) disarm(processInfo.hProcess, sboxSeedSourceReturn);
     if (generatorReturn.installed) disarm(processInfo.hProcess, generatorReturn);
     if (tableBuildReturn.installed) disarm(processInfo.hProcess, tableBuildReturn);
    for (auto& breakpoint : g_d0WriteBreakpoints) {
        if (breakpoint.installed) disarm(processInfo.hProcess, breakpoint);
    }
    clearTableWatches();
    for (auto& site : dispatchSites) {
        if (site.installed) disarm(processInfo.hProcess, site);
    }
    CloseHandle(processInfo.hThread);
    CloseHandle(processInfo.hProcess);
    return 0;
}
