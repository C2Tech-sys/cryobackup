/*
 * cryoca.dll - the CryoBackup MSI's one custom-action entry point.
 *
 * Why a DLL at all: a deferred custom action of the EXE kind receives its data
 * only on its command line, and the enrollment token must never be on a
 * command line (any process that can query the CA's process can read it).
 * A DLL custom action can read its CustomActionData from the session, so this
 * DLL does exactly that and nothing more:
 *
 *   1. read CustomActionData, which the package sets (deploy/windows/CryoBackup.wxs) to
 *        exe=<full path of cryo-agent.exe>
 *        action=configure | rollback | commit
 *        key=value ...            (the request for cryo-agent, see cmd/cryo-agent/msi.go)
 *   2. start  "<exe>" msi-<action>  with no other argument, its stdin a pipe;
 *   3. write the request lines to that pipe and close it (the token travels
 *      there and nowhere else: not argv, not the environment, not a file);
 *   4. copy every line cryo-agent prints into the MSI log;
 *   5. fail the action when cryo-agent exits non-zero.
 *
 * The package marks the CustomActionData property hidden and the action
 * HideTarget, so Windows Installer never writes the data into its log either.
 * Buffers that held the data are zeroed before they are freed.
 *
 * Build (MinGW-w64, deploy/windows/build.ps1 does this):
 *   gcc -O2 -shared -s -static-libgcc -o cryoca.dll cryoca.c -lmsi -lbcrypt
 *
 * A second, immediate entry point, CryoNewNonce, gives each install run its
 * own nonce (see below).
 */
#define WIN32_LEAN_AND_MEAN
#define UNICODE
#define _UNICODE
#include <windows.h>
#include <msi.h>
#include <msiquery.h>
#include <bcrypt.h>
#include <wchar.h>

#define LOG_PREFIX L"CryoBackup: "

/* logLine writes one line to the MSI log (INSTALLMESSAGE_INFO). */
static void logLine(MSIHANDLE h, const wchar_t *text)
{
    MSIHANDLE rec = MsiCreateRecord(1);
    if (!rec)
        return;
    size_t n = lstrlenW(LOG_PREFIX) + lstrlenW(text) + 1;
    wchar_t *buf = (wchar_t *)HeapAlloc(GetProcessHeap(), 0, n * sizeof(wchar_t));
    if (buf) {
        lstrcpyW(buf, LOG_PREFIX);
        lstrcatW(buf, text);
        /* Field 0 is the template; "[1]" would be expanded, so the text goes
           in field 1 and the template only references it. */
        MsiRecordSetStringW(rec, 0, L"[1]");
        MsiRecordSetStringW(rec, 1, buf);
        MsiProcessMessage(h, INSTALLMESSAGE_INFO, rec);
        HeapFree(GetProcessHeap(), 0, buf);
    }
    MsiCloseHandle(rec);
}

/* logUTF8 converts one UTF-8 line from cryo-agent and logs it. */
static void logUTF8(MSIHANDLE h, const char *s, int len)
{
    if (len <= 0)
        return;
    int n = MultiByteToWideChar(CP_UTF8, 0, s, len, NULL, 0);
    if (n <= 0)
        return;
    wchar_t *w = (wchar_t *)HeapAlloc(GetProcessHeap(), 0, (n + 1) * sizeof(wchar_t));
    if (!w)
        return;
    MultiByteToWideChar(CP_UTF8, 0, s, len, w, n);
    w[n] = 0;
    logLine(h, w);
    HeapFree(GetProcessHeap(), 0, w);
}

/* failWith logs a fixed message and an error number. */
static UINT failWith(MSIHANDLE h, const wchar_t *what, DWORD err)
{
    wchar_t msg[256];
    wsprintfW(msg, L"%s (error %lu)", what, (unsigned long)err);
    logLine(h, msg);
    return ERROR_INSTALL_FAILURE;
}

static void wipeFree(void *p, size_t bytes)
{
    if (p) {
        SecureZeroMemory(p, bytes);
        HeapFree(GetProcessHeap(), 0, p);
    }
}

/* takeLine returns the next line of *cur (terminated in place) and advances
   *cur past it; NULL at the end. */
static wchar_t *takeLine(wchar_t **cur)
{
    wchar_t *s = *cur;
    if (!s || !*s)
        return NULL;
    wchar_t *e = s;
    while (*e && *e != L'\n')
        e++;
    if (*e) {
        *e = 0;
        *cur = e + 1;
    } else {
        *cur = e;
    }
    if (e > s && e[-1] == L'\r')
        e[-1] = 0;
    return s;
}

static int startsWith(const wchar_t *s, const wchar_t *prefix)
{
    return wcsncmp(s, prefix, wcslen(prefix)) == 0;
}

__declspec(dllexport) UINT __stdcall CryoAgentAction(MSIHANDLE h)
{
    UINT rc = ERROR_INSTALL_FAILURE;
    wchar_t *data = NULL;
    DWORD dataLen = 0;
    char *req = NULL;
    int reqLen = 0;
    wchar_t *cmd = NULL;
    HANDLE inR = NULL, inW = NULL, outR = NULL, outW = NULL;
    LPPROC_THREAD_ATTRIBUTE_LIST attrs = NULL;
    PROCESS_INFORMATION pi = {0};

    /* 1. CustomActionData. */
    wchar_t probe[1] = L"";
    UINT r = MsiGetPropertyW(h, L"CustomActionData", probe, &dataLen);
    if (r != ERROR_MORE_DATA && r != ERROR_SUCCESS)
        return failWith(h, L"CustomActionData could not be read", r);
    dataLen++;
    data = (wchar_t *)HeapAlloc(GetProcessHeap(), HEAP_ZERO_MEMORY, dataLen * sizeof(wchar_t));
    if (!data)
        return failWith(h, L"out of memory", ERROR_OUTOFMEMORY);
    DWORD got = dataLen;
    if ((r = MsiGetPropertyW(h, L"CustomActionData", data, &got)) != ERROR_SUCCESS) {
        rc = failWith(h, L"CustomActionData could not be read", r);
        goto done;
    }

    wchar_t *cur = data;
    wchar_t *exeLine = takeLine(&cur);
    wchar_t *actLine = takeLine(&cur);
    if (!exeLine || !actLine || !startsWith(exeLine, L"exe=") || !startsWith(actLine, L"action=")) {
        rc = failWith(h, L"CustomActionData is malformed", ERROR_INVALID_DATA);
        goto done;
    }
    const wchar_t *exe = exeLine + 4;
    const wchar_t *act = actLine + 7;
    const wchar_t *sub = NULL;
    if (lstrcmpW(act, L"configure") == 0)
        sub = L"msi-configure";
    else if (lstrcmpW(act, L"rollback") == 0)
        sub = L"msi-rollback";
    else if (lstrcmpW(act, L"commit") == 0)
        sub = L"msi-commit";
    if (!sub || !*exe || wcschr(exe, L'"')) {
        rc = failWith(h, L"CustomActionData names no valid action or executable", ERROR_INVALID_DATA);
        goto done;
    }

    /* The rest of the data, as UTF-8, is the request on stdin. */
    int restChars = lstrlenW(cur);
    if (restChars > 0) {
        reqLen = WideCharToMultiByte(CP_UTF8, 0, cur, restChars, NULL, 0, NULL, NULL);
        req = (char *)HeapAlloc(GetProcessHeap(), HEAP_ZERO_MEMORY, reqLen + 1);
        if (!req) {
            rc = failWith(h, L"out of memory", ERROR_OUTOFMEMORY);
            goto done;
        }
        WideCharToMultiByte(CP_UTF8, 0, cur, restChars, req, reqLen, NULL, NULL);
    }
    /* The wide copy of the request is not needed any more: wipe it now (the
       executable path before it is still used). */
    SecureZeroMemory(cur, (size_t)restChars * sizeof(wchar_t));

    /* 2. Pipes. Only the child's ends are inheritable. */
    SECURITY_ATTRIBUTES sa = {sizeof(sa), NULL, TRUE};
    if (!CreatePipe(&inR, &inW, &sa, 65536) || !CreatePipe(&outR, &outW, &sa, 65536)) {
        rc = failWith(h, L"pipes could not be created", GetLastError());
        goto done;
    }
    SetHandleInformation(inW, HANDLE_FLAG_INHERIT, 0);
    SetHandleInformation(outR, HANDLE_FLAG_INHERIT, 0);

    /* Inherit exactly these two handles, not whatever else msiexec holds. */
    SIZE_T attrSize = 0;
    InitializeProcThreadAttributeList(NULL, 1, 0, &attrSize);
    attrs = (LPPROC_THREAD_ATTRIBUTE_LIST)HeapAlloc(GetProcessHeap(), 0, attrSize);
    HANDLE inherit[2] = {inR, outW};
    if (!attrs || !InitializeProcThreadAttributeList(attrs, 1, 0, &attrSize) ||
        !UpdateProcThreadAttribute(attrs, 0, PROC_THREAD_ATTRIBUTE_HANDLE_LIST, inherit, sizeof(inherit), NULL, NULL)) {
        rc = failWith(h, L"the process attributes could not be set", GetLastError());
        goto done;
    }

    /* "<exe>" msi-<action> */
    size_t cmdChars = lstrlenW(exe) + lstrlenW(sub) + 4;
    cmd = (wchar_t *)HeapAlloc(GetProcessHeap(), HEAP_ZERO_MEMORY, cmdChars * sizeof(wchar_t));
    if (!cmd) {
        rc = failWith(h, L"out of memory", ERROR_OUTOFMEMORY);
        goto done;
    }
    wsprintfW(cmd, L"\"%s\" %s", exe, sub);

    STARTUPINFOEXW si;
    ZeroMemory(&si, sizeof(si));
    si.StartupInfo.cb = sizeof(si);
    si.StartupInfo.dwFlags = STARTF_USESTDHANDLES;
    si.StartupInfo.hStdInput = inR;
    si.StartupInfo.hStdOutput = outW;
    si.StartupInfo.hStdError = outW;
    si.lpAttributeList = attrs;
    if (!CreateProcessW(exe, cmd, NULL, NULL, TRUE, CREATE_NO_WINDOW | EXTENDED_STARTUPINFO_PRESENT, NULL, NULL,
                        &si.StartupInfo, &pi)) {
        rc = failWith(h, L"cryo-agent.exe could not be started", GetLastError());
        goto done;
    }
    CloseHandle(inR);
    inR = NULL;
    CloseHandle(outW);
    outW = NULL;

    /* 3. The request, then EOF. It is far smaller than the pipe buffer, so
       writing it all before reading cannot deadlock. */
    if (req && reqLen > 0) {
        DWORD wrote = 0;
        if (!WriteFile(inW, req, (DWORD)reqLen, &wrote, NULL) || wrote != (DWORD)reqLen)
            logLine(h, L"the request could not be written to cryo-agent");
    }
    CloseHandle(inW);
    inW = NULL;
    wipeFree(req, (size_t)reqLen + 1);
    req = NULL;

    /* 4. Every output line into the log. */
    char buf[4096];
    char line[2048];
    int lineLen = 0;
    DWORD n = 0;
    while (ReadFile(outR, buf, sizeof(buf), &n, NULL) && n > 0) {
        for (DWORD i = 0; i < n; i++) {
            char c = buf[i];
            if (c == '\n' || lineLen == (int)sizeof(line) - 1) {
                if (lineLen > 0 && line[lineLen - 1] == '\r')
                    lineLen--;
                logUTF8(h, line, lineLen);
                lineLen = 0;
                if (c == '\n')
                    continue;
            }
            line[lineLen++] = c;
        }
    }
    logUTF8(h, line, lineLen);

    /* 5. The verdict. */
    WaitForSingleObject(pi.hProcess, INFINITE);
    DWORD code = 1;
    GetExitCodeProcess(pi.hProcess, &code);
    if (code == 0) {
        rc = ERROR_SUCCESS;
    } else {
        wchar_t msg[160];
        wsprintfW(msg, L"cryo-agent %s failed with exit code %lu; the lines above say why", sub, (unsigned long)code);
        logLine(h, msg);
        rc = ERROR_INSTALL_FAILURE;
    }

done:
    if (pi.hThread)
        CloseHandle(pi.hThread);
    if (pi.hProcess)
        CloseHandle(pi.hProcess);
    if (inR)
        CloseHandle(inR);
    if (inW)
        CloseHandle(inW);
    if (outR)
        CloseHandle(outR);
    if (outW)
        CloseHandle(outW);
    if (attrs) {
        DeleteProcThreadAttributeList(attrs);
        HeapFree(GetProcessHeap(), 0, attrs);
    }
    if (cmd)
        HeapFree(GetProcessHeap(), 0, cmd);
    wipeFree(req, (size_t)reqLen + 1);
    wipeFree(data, (size_t)dataLen * sizeof(wchar_t));
    return rc;
}

/*
 * CryoNewNonce - immediate action, before the install script is built: sets
 * the private property CryoRunNonce to 16 random bytes in lower-case hex. The
 * package passes it to both the configure and the rollback action of this
 * run, so a rollback can recognise a state directory that THIS run created
 * (cmd/cryo-agent/msi.go) and never removes one it did not. It is an
 * identifier, not a secret.
 */
__declspec(dllexport) UINT __stdcall CryoNewNonce(MSIHANDLE h)
{
    unsigned char b[16];
    LONG st = BCryptGenRandom(NULL, b, sizeof(b), BCRYPT_USE_SYSTEM_PREFERRED_RNG);
    if (st != 0)
        return failWith(h, L"no random nonce could be generated", (DWORD)st);
    static const wchar_t hex[] = L"0123456789abcdef";
    wchar_t s[33];
    for (int i = 0; i < 16; i++) {
        s[2 * i] = hex[b[i] >> 4];
        s[2 * i + 1] = hex[b[i] & 15];
    }
    s[32] = 0;
    UINT r = MsiSetPropertyW(h, L"CryoRunNonce", s);
    if (r != ERROR_SUCCESS)
        return failWith(h, L"CryoRunNonce could not be set", r);
    return ERROR_SUCCESS;
}

BOOL WINAPI DllMain(HINSTANCE inst, DWORD reason, LPVOID reserved)
{
    (void)inst;
    (void)reason;
    (void)reserved;
    return TRUE;
}
