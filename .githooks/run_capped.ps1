$scriptArgs = @($args)
$SelfTest = $scriptArgs -contains "-SelfTest"
$SelfTestArchitecture = "auto"
if ($SelfTest) {
    $remainingArgs = [Collections.Generic.List[string]]::new()
    for ($index = 0; $index -lt $scriptArgs.Count; $index++) {
        if ($scriptArgs[$index] -eq "-SelfTest") {
            continue
        }
        if ($scriptArgs[$index] -in @("-Architecture", "-SelfTestArchitecture")) {
            if ($index + 1 -ge $scriptArgs.Count) {
                throw "$($scriptArgs[$index]) requires x64 or x86"
            }
            $SelfTestArchitecture = [string]$scriptArgs[++$index]
            continue
        }
        $remainingArgs.Add([string]$scriptArgs[$index])
    }
    $scriptArgs = @($remainingArgs)
}
if (-not $SelfTest) {
    if ($scriptArgs.Count -lt 2) {
        throw "Usage: run_capped.ps1 <memory> <timeout-seconds> <command> [args...]"
    }
    $MemoryLimit = [string] $scriptArgs[0]
    $TimeoutSeconds = [int] $scriptArgs[1]
    $Command = @($scriptArgs[2..($scriptArgs.Count - 1)])
}

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

if ($SelfTestArchitecture -notin @("auto", "x64", "x86")) {
    throw "Self-test architecture must be auto, x64, or x86: $SelfTestArchitecture"
}
if ($SelfTest -and $SelfTestArchitecture -eq "x64" -and -not [Environment]::Is64BitProcess) {
    throw "x64 self-test requires native x64 PowerShell"
}
if ($SelfTest -and $SelfTestArchitecture -eq "x86" -and [Environment]::Is64BitProcess) {
    throw "x86 self-test requires SysWOW64 32-bit PowerShell"
}

Add-Type @'
using System;
using System.ComponentModel;
using System.IO;
using System.Runtime.InteropServices;
using System.Text;
using System.Threading;
using System.Threading.Tasks;
using Microsoft.Win32.SafeHandles;

public static class RushJobObject {
    [StructLayout(LayoutKind.Sequential)]
    private struct JOBOBJECT_BASIC_LIMIT_INFORMATION {
        public long PerProcessUserTimeLimit;
        public long PerJobUserTimeLimit;
        public uint LimitFlags;
        public UIntPtr MinimumWorkingSetSize;
        public UIntPtr MaximumWorkingSetSize;
        public uint ActiveProcessLimit;
        public UIntPtr Affinity;
        public uint PriorityClass;
        public uint SchedulingClass;
    }

    [StructLayout(LayoutKind.Sequential)]
    private struct IO_COUNTERS {
        public ulong ReadOperationCount;
        public ulong WriteOperationCount;
        public ulong OtherOperationCount;
        public ulong ReadTransferCount;
        public ulong WriteTransferCount;
        public ulong OtherTransferCount;
    }

    [StructLayout(LayoutKind.Sequential)]
    private struct JOBOBJECT_EXTENDED_LIMIT_INFORMATION {
        public JOBOBJECT_BASIC_LIMIT_INFORMATION BasicLimitInformation;
        public IO_COUNTERS IoInfo;
        public UIntPtr ProcessMemoryLimit;
        public UIntPtr JobMemoryLimit;
        public UIntPtr PeakProcessMemoryUsed;
        public UIntPtr PeakJobMemoryUsed;
    }

    [StructLayout(LayoutKind.Sequential)]
    private struct SECURITY_ATTRIBUTES {
        public int Length;
        public IntPtr SecurityDescriptor;
        public int InheritHandle;
    }

    [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)]
    private struct STARTUPINFO {
        public int Size;
        public string Reserved;
        public string Desktop;
        public string Title;
        public int X;
        public int Y;
        public int XSize;
        public int YSize;
        public int XCountChars;
        public int YCountChars;
        public int FillAttribute;
        public int Flags;
        public short ShowWindow;
        public short Reserved2;
        public IntPtr Reserved2Data;
        public IntPtr StandardInput;
        public IntPtr StandardOutput;
        public IntPtr StandardError;
    }

    [StructLayout(LayoutKind.Sequential)]
    private struct PROCESS_INFORMATION {
        public IntPtr Process;
        public IntPtr Thread;
        public int ProcessId;
        public int ThreadId;
    }

    [StructLayout(LayoutKind.Sequential)]
    private struct STARTUPINFOEX {
        public STARTUPINFO StartupInfo;
        public IntPtr AttributeList;
    }

    private const uint JobObjectExtendedLimitInformation = 9;
    private const uint JobObjectLimitJobMemory = 0x200;
    private const uint JobObjectLimitKillOnJobClose = 0x2000;
    private const uint CreateSuspendedFlag = 0x00000004;
    private const uint CreateNoWindow = 0x08000000;
    private const uint ExtendedStartupinfoPresent = 0x00080000;
    private const int StartfUseStdHandles = 0x00000100;
    private const uint HandleFlagInherit = 0x00000001;
    private const int StdInputHandle = -10;
    private const uint DuplicateSameAccess = 0x00000002;
    private const uint GenericRead = 0x80000000;
    private const uint FileShareRead = 0x00000001;
    private const uint FileShareWrite = 0x00000002;
    private const uint CreateNew = 1;
    private const uint OpenExisting = 3;
    private const uint ErrorInsufficientBuffer = 122;
    private const uint ProcThreadAttributeHandleList = 0x00020002;
    private const uint WaitObject0 = 0;
    private const uint WaitTimeout = 0x00000102;
    private const uint WaitFailed = 0xffffffff;

    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    private static extern IntPtr CreateJobObject(IntPtr attributes, string name);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool SetInformationJobObject(
        IntPtr job, uint infoClass, IntPtr info, uint length);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool AssignProcessToJobObject(IntPtr job, IntPtr process);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool CreatePipe(
        out IntPtr readPipe, out IntPtr writePipe,
        ref SECURITY_ATTRIBUTES attributes, uint size);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool SetHandleInformation(
        IntPtr handle, uint mask, uint flags);

    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    private static extern bool CreateProcess(
        string applicationName, StringBuilder commandLine,
        IntPtr processAttributes, IntPtr threadAttributes, bool inheritHandles,
        uint creationFlags, IntPtr environment, string currentDirectory,
        ref STARTUPINFOEX startupInfo, out PROCESS_INFORMATION processInfo);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool InitializeProcThreadAttributeList(
        IntPtr attributeList, int attributeCount, uint flags, ref IntPtr size);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool UpdateProcThreadAttribute(
        IntPtr attributeList, uint flags, uint attribute, IntPtr value,
        IntPtr size, IntPtr previousValue, IntPtr returnSize);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern void DeleteProcThreadAttributeList(IntPtr attributeList);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern uint ResumeThread(IntPtr thread);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern uint WaitForSingleObject(IntPtr handle, uint timeout);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool TerminateProcess(IntPtr process, uint exitCode);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool TerminateJobObject(IntPtr job, uint exitCode);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool GetExitCodeProcess(IntPtr process, out uint exitCode);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern IntPtr GetStdHandle(int standardHandle);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern IntPtr GetCurrentProcess();

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool DuplicateHandle(
        IntPtr sourceProcess, IntPtr sourceHandle, IntPtr targetProcess,
        out IntPtr targetHandle, uint desiredAccess, bool inheritHandle,
        uint options);

    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    private static extern IntPtr CreateFile(
        string name, uint desiredAccess, uint shareMode,
        ref SECURITY_ATTRIBUTES securityAttributes, uint creationDisposition,
        uint flags, IntPtr template);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool CloseHandle(IntPtr handle);

    private static void CloseIfOpen(ref IntPtr handle) {
        if (handle != IntPtr.Zero) {
            CloseHandle(handle);
            handle = IntPtr.Zero;
        }
    }

    private static Win32Exception LastError() {
        return new Win32Exception(Marshal.GetLastWin32Error());
    }

    private static bool IsInvalidHandle(IntPtr handle) {
        return handle == IntPtr.Zero || handle == new IntPtr(-1);
    }

    private static IntPtr DuplicateOrOpenStandardInput() {
        IntPtr standardInput = GetStdHandle(StdInputHandle);
        IntPtr input;
        if (!IsInvalidHandle(standardInput)) {
            if (!DuplicateHandle(GetCurrentProcess(), standardInput,
                    GetCurrentProcess(), out input, 0, true, DuplicateSameAccess))
                throw LastError();
            return input;
        }

        var attributes = new SECURITY_ATTRIBUTES {
            Length = Marshal.SizeOf(typeof(SECURITY_ATTRIBUTES)),
            InheritHandle = 1
        };
        input = CreateFile("NUL", GenericRead, FileShareRead | FileShareWrite,
            ref attributes, OpenExisting, 0, IntPtr.Zero);
        if (IsInvalidHandle(input)) throw LastError();
        return input;
    }

    public static IntPtr Create(ulong memoryBytes) {
        IntPtr job = CreateJobObject(IntPtr.Zero, null);
        if (job == IntPtr.Zero) throw LastError();
        try {
            ValidateMemoryLimit(memoryBytes);
            var limits = new JOBOBJECT_EXTENDED_LIMIT_INFORMATION();
            limits.BasicLimitInformation.LimitFlags =
                JobObjectLimitJobMemory | JobObjectLimitKillOnJobClose;
            limits.JobMemoryLimit = new UIntPtr(memoryBytes);
            int size = Marshal.SizeOf(typeof(JOBOBJECT_EXTENDED_LIMIT_INFORMATION));
            IntPtr buffer = Marshal.AllocHGlobal(size);
            try {
                Marshal.StructureToPtr(limits, buffer, false);
                if (!SetInformationJobObject(job, JobObjectExtendedLimitInformation,
                        buffer, (uint)size)) throw LastError();
            } finally {
                Marshal.FreeHGlobal(buffer);
            }
            return job;
        } catch {
            CloseIfOpen(ref job);
            throw;
        }
    }

    public static void ValidateMemoryLimit(ulong memoryBytes) {
        if (IntPtr.Size == 4 && memoryBytes > uint.MaxValue)
            throw new ArgumentOutOfRangeException("memoryBytes", "The memory limit does not fit in a 32-bit SIZE_T.");
        var value = new UIntPtr(memoryBytes);
        if (value.ToUInt64() != memoryBytes)
            throw new ArgumentOutOfRangeException("memoryBytes", "The memory limit was truncated to the native SIZE_T.");
    }

    public static void ValidateLayouts(ulong memoryBytes) {
        int expectedBasic = IntPtr.Size == 8 ? 64 : 48;
        int expectedExtended = IntPtr.Size == 8 ? 144 : 112;
        int expectedStartup = IntPtr.Size == 8 ? 104 : 68;
        int expectedStartupEx = IntPtr.Size == 8 ? 112 : 72;
        int expectedProcess = IntPtr.Size == 8 ? 24 : 16;
        int expectedSecurity = IntPtr.Size == 8 ? 24 : 12;
        if (Marshal.SizeOf(typeof(JOBOBJECT_BASIC_LIMIT_INFORMATION)) != expectedBasic ||
                Marshal.SizeOf(typeof(JOBOBJECT_EXTENDED_LIMIT_INFORMATION)) != expectedExtended ||
                Marshal.SizeOf(typeof(STARTUPINFO)) != expectedStartup ||
                Marshal.SizeOf(typeof(STARTUPINFOEX)) != expectedStartupEx ||
                Marshal.SizeOf(typeof(PROCESS_INFORMATION)) != expectedProcess ||
                Marshal.SizeOf(typeof(SECURITY_ATTRIBUTES)) != expectedSecurity ||
                Marshal.OffsetOf(typeof(STARTUPINFOEX), "AttributeList").ToInt32() != expectedStartup) {
            throw new InvalidOperationException(
                "Native interop struct layout does not match the current process architecture: " +
                Marshal.SizeOf(typeof(JOBOBJECT_BASIC_LIMIT_INFORMATION)) + "/" +
                Marshal.SizeOf(typeof(JOBOBJECT_EXTENDED_LIMIT_INFORMATION)) + "/" +
                Marshal.SizeOf(typeof(STARTUPINFO)) + "/" +
                Marshal.SizeOf(typeof(STARTUPINFOEX)) + "/" +
                Marshal.SizeOf(typeof(PROCESS_INFORMATION)) + "/" +
                Marshal.SizeOf(typeof(SECURITY_ATTRIBUTES)) + "/" +
                Marshal.OffsetOf(typeof(STARTUPINFOEX), "AttributeList"));
        }
        ValidateMemoryLimit(memoryBytes);
        if (IntPtr.Size != UIntPtr.Size)
            throw new InvalidOperationException("UIntPtr does not match the native pointer size.");
    }

    public static IntPtr CreateInheritableSentinel() {
        var attributes = new SECURITY_ATTRIBUTES {
            Length = Marshal.SizeOf(typeof(SECURITY_ATTRIBUTES)),
            InheritHandle = 1
        };
        IntPtr handle = CreateFile("NUL", GenericRead,
            FileShareRead | FileShareWrite, ref attributes, OpenExisting, 0,
            IntPtr.Zero);
        if (IsInvalidHandle(handle)) throw LastError();
        return handle;
    }

    public static IntPtr CreateInheritableSentinel(string path) {
        var attributes = new SECURITY_ATTRIBUTES {
            Length = Marshal.SizeOf(typeof(SECURITY_ATTRIBUTES)),
            InheritHandle = 1
        };
        IntPtr handle = CreateFile(path, GenericRead,
            FileShareRead | FileShareWrite, ref attributes, CreateNew, 0,
            IntPtr.Zero);
        if (IsInvalidHandle(handle)) throw LastError();
        return handle;
    }

    public static void CreateSuspended(
        string commandLine,
        out IntPtr process,
        out IntPtr thread,
        out IntPtr stdoutRead,
        out IntPtr stderrRead) {
        process = IntPtr.Zero;
        thread = IntPtr.Zero;
        stdoutRead = IntPtr.Zero;
        stderrRead = IntPtr.Zero;
        IntPtr stdoutWrite = IntPtr.Zero;
        IntPtr stderrWrite = IntPtr.Zero;
        IntPtr stdin = IntPtr.Zero;
        IntPtr attributeList = IntPtr.Zero;
        IntPtr attributeHandles = IntPtr.Zero;
        bool attributeListInitialized = false;
        try {
            var attributes = new SECURITY_ATTRIBUTES {
                Length = Marshal.SizeOf(typeof(SECURITY_ATTRIBUTES)),
                InheritHandle = 1
            };
            if (!CreatePipe(out stdoutRead, out stdoutWrite, ref attributes, 0))
                throw LastError();
            if (!SetHandleInformation(stdoutRead, HandleFlagInherit, 0))
                throw LastError();
            if (!CreatePipe(out stderrRead, out stderrWrite, ref attributes, 0))
                throw LastError();
            if (!SetHandleInformation(stderrRead, HandleFlagInherit, 0))
                throw LastError();
            stdin = DuplicateOrOpenStandardInput();

            int handleCount = 3;
            IntPtr attributeListSize = IntPtr.Zero;
            if (InitializeProcThreadAttributeList(IntPtr.Zero, 1, 0,
                    ref attributeListSize) ||
                    Marshal.GetLastWin32Error() != ErrorInsufficientBuffer ||
                    attributeListSize == IntPtr.Zero)
                throw LastError();
            attributeList = Marshal.AllocHGlobal(attributeListSize.ToInt32());
            if (!InitializeProcThreadAttributeList(attributeList, 1, 0,
                    ref attributeListSize))
                throw LastError();
            attributeListInitialized = true;
            attributeHandles = Marshal.AllocHGlobal(IntPtr.Size * handleCount);
            Marshal.WriteIntPtr(attributeHandles, 0, stdin);
            Marshal.WriteIntPtr(attributeHandles, IntPtr.Size, stdoutWrite);
            Marshal.WriteIntPtr(attributeHandles, IntPtr.Size * 2, stderrWrite);
            if (!UpdateProcThreadAttribute(attributeList, 0,
                    ProcThreadAttributeHandleList, attributeHandles,
                    (IntPtr)(IntPtr.Size * handleCount), IntPtr.Zero, IntPtr.Zero))
                throw LastError();

            var startupInfo = new STARTUPINFOEX {
                StartupInfo = new STARTUPINFO {
                    Size = Marshal.SizeOf(typeof(STARTUPINFOEX)),
                    Flags = StartfUseStdHandles,
                    StandardInput = stdin,
                    StandardOutput = stdoutWrite,
                    StandardError = stderrWrite
                },
                AttributeList = attributeList
            };
            var command = new StringBuilder(commandLine);
            PROCESS_INFORMATION processInfo;
            if (!CreateProcess(null, command, IntPtr.Zero, IntPtr.Zero, true,
                    CreateSuspendedFlag | CreateNoWindow | ExtendedStartupinfoPresent,
                    IntPtr.Zero, null,
                    ref startupInfo, out processInfo)) {
                throw LastError();
            }
            process = processInfo.Process;
            thread = processInfo.Thread;
            CloseIfOpen(ref stdoutWrite);
            CloseIfOpen(ref stderrWrite);
            CloseIfOpen(ref stdin);
        } catch {
            CloseIfOpen(ref process);
            CloseIfOpen(ref thread);
            CloseIfOpen(ref stdoutRead);
            CloseIfOpen(ref stderrRead);
            throw;
        } finally {
            if (attributeListInitialized)
                DeleteProcThreadAttributeList(attributeList);
            if (attributeHandles != IntPtr.Zero)
                Marshal.FreeHGlobal(attributeHandles);
            if (attributeList != IntPtr.Zero)
                Marshal.FreeHGlobal(attributeList);
            CloseIfOpen(ref stdoutWrite);
            CloseIfOpen(ref stderrWrite);
            CloseIfOpen(ref stdin);
        }
    }

    public static void Assign(IntPtr job, IntPtr process) {
        if (!AssignProcessToJobObject(job, process)) throw LastError();
    }

    public static void Resume(IntPtr thread) {
        if (ResumeThread(thread) == 0xffffffff) throw LastError();
    }

    public static bool Wait(IntPtr process, uint timeoutMilliseconds) {
        uint result = WaitForSingleObject(process, timeoutMilliseconds);
        if (result == WaitFailed) throw LastError();
        if (result == WaitTimeout) return false;
        if (result != WaitObject0) throw new InvalidOperationException("Unexpected process wait result");
        return true;
    }

    public static int ExitCode(IntPtr process) {
        uint exitCode;
        if (!GetExitCodeProcess(process, out exitCode)) throw LastError();
        return unchecked((int)exitCode);
    }

    public static void Terminate(IntPtr process, uint exitCode) {
        if (!TerminateProcess(process, exitCode)) throw LastError();
    }

    public static void TerminateJob(IntPtr job, uint exitCode) {
        if (!TerminateJobObject(job, exitCode)) throw LastError();
    }

    public static void Close(IntPtr handle) {
        if (handle != IntPtr.Zero) CloseHandle(handle);
    }

    public static string QuoteArgument(string value) {
        if (!string.IsNullOrEmpty(value)) {
            bool needsQuotes = false;
            foreach (char character in value) {
                if (char.IsWhiteSpace(character) || character == '"') {
                    needsQuotes = true;
                    break;
                }
            }
            if (!needsQuotes) return value;
        }
        var quoted = new StringBuilder();
        quoted.Append('"');
        int backslashes = 0;
        foreach (char character in value ?? string.Empty) {
            if (character == '\\') {
                backslashes++;
            } else if (character == '"') {
                quoted.Append('\\', backslashes * 2 + 1);
                quoted.Append('"');
                backslashes = 0;
            } else {
                quoted.Append('\\', backslashes);
                quoted.Append(character);
                backslashes = 0;
            }
        }
        quoted.Append('\\', backslashes * 2);
        quoted.Append('"');
        return quoted.ToString();
    }

    public static string BuildCommandLine(string[] arguments) {
        var commandLine = new StringBuilder();
        for (int i = 0; i < arguments.Length; i++) {
            if (i != 0) commandLine.Append(' ');
            commandLine.Append(QuoteArgument(arguments[i]));
        }
        return commandLine.ToString();
    }
}

public static class RushOutputPump {
    public const int BufferSize = 8192;

    internal sealed class LifecycleHooks {
        public Action BeforeSettle;
        public Action Settled;
        public Action DisposeStarted;
        public Action DisposeCompleted;
    }

    public static Encoding WithoutPreamble(Encoding encoding) {
        if (encoding == null) throw new ArgumentNullException("encoding");
        switch (encoding.CodePage) {
            case 65001: return new UTF8Encoding(false);
            case 1200: return new UnicodeEncoding(false, false);
            case 1201: return new UnicodeEncoding(true, false);
            case 12000: return new UTF32Encoding(false, false);
            case 12001: return new UTF32Encoding(true, false);
            default: return encoding;
        }
    }

    public sealed class Pump : IDisposable {
        private readonly StreamReader reader;
        private readonly TextWriter destination;
        private readonly CancellationTokenSource cancellation = new CancellationTokenSource();
        private readonly Task task;
        private readonly object state = new object();
        private readonly LifecycleHooks hooks;
        private int disposed;
        private bool settled;
        private bool disposeComplete;
        private bool ctsDisposed;

        internal Pump(StreamReader reader, TextWriter destination) :
            this(reader, destination, null) { }

        internal Pump(StreamReader reader, TextWriter destination, LifecycleHooks hooks) {
            this.reader = reader;
            this.destination = destination;
            this.hooks = hooks;
            try {
                task = Task.Run(() => {
                        try {
                            Copy(reader, destination, cancellation.Token);
                        } finally {
                            reader.Dispose();
                            Settle();
                        }
                    });
                task.ContinueWith(completed => { var ignored = completed.Exception; },
                    CancellationToken.None, TaskContinuationOptions.OnlyOnFaulted |
                    TaskContinuationOptions.ExecuteSynchronously, TaskScheduler.Default);
            } catch {
                reader.Dispose();
                cancellation.Dispose();
                throw;
            }
        }

        private void Settle() {
            if (hooks != null && hooks.BeforeSettle != null) hooks.BeforeSettle();
            bool disposeCancellation;
            lock (state) {
                settled = true;
                disposeCancellation = disposeComplete && !ctsDisposed;
                if (disposeCancellation) ctsDisposed = true;
            }
            if (hooks != null && hooks.Settled != null) hooks.Settled();
            if (disposeCancellation) cancellation.Dispose();
        }

        public bool Wait(int timeoutMilliseconds) {
            if (!task.Wait(timeoutMilliseconds)) return false;
            task.GetAwaiter().GetResult();
            return true;
        }

        public void Wait() {
            task.GetAwaiter().GetResult();
        }

        public void Dispose() {
            bool performDispose;
            lock (state) {
                performDispose = disposed == 0;
                if (performDispose) disposed = 1;
            }
            if (performDispose) {
                if (hooks != null && hooks.DisposeStarted != null) hooks.DisposeStarted();
                try {
                    cancellation.Cancel();
                } finally {
                    try {
                        reader.Dispose();
                    } finally {
                        FinishDispose();
                    }
                }
            }
            if (task.IsCompleted) task.GetAwaiter().GetResult();
        }

        private void FinishDispose() {
            bool disposeCancellation;
            lock (state) {
                disposeComplete = true;
                disposeCancellation = settled && !ctsDisposed;
                if (disposeCancellation) ctsDisposed = true;
            }
            if (disposeCancellation) cancellation.Dispose();
            if (hooks != null && hooks.DisposeCompleted != null) hooks.DisposeCompleted();
        }
    }

    private static void Copy(StreamReader reader, TextWriter destination,
        CancellationToken cancellation) {
        char[] buffer = new char[BufferSize];
        int read;
        while ((read = reader.Read(buffer, 0, buffer.Length)) != 0) {
            cancellation.ThrowIfCancellationRequested();
            destination.Write(buffer, 0, read);
        }
        destination.Flush();
    }

    private static Pump StartOwned(Stream source, TextWriter destination, Encoding encoding,
        LifecycleHooks hooks) {
        if (source == null) throw new ArgumentNullException("source");
        if (destination == null) throw new ArgumentNullException("destination");
        if (encoding == null) throw new ArgumentNullException("encoding");
        StreamReader reader = null;
        try {
            reader = new StreamReader(source, encoding, true, BufferSize, false);
            source = null;
            return new Pump(reader, destination, hooks);
        } catch {
            if (reader != null) reader.Dispose();
            if (source != null) source.Dispose();
            throw;
        }
    }

    public static Pump Start(Stream source, TextWriter destination, Encoding encoding) {
        return StartOwned(source, destination, encoding, null);
    }

    private static Pump StartLifecycleTest(Stream source, TextWriter destination,
        Encoding encoding, LifecycleHooks hooks) {
        return StartOwned(source, destination, encoding, hooks);
    }

    public static Pump Start(IntPtr handle, TextWriter destination, Encoding encoding) {
        if (handle == IntPtr.Zero || handle == new IntPtr(-1))
            throw new ArgumentException("The pipe read handle is invalid.", "handle");
        var safeHandle = new SafeFileHandle(handle, true);
        try {
            var source = new FileStream(safeHandle, FileAccess.Read, BufferSize, false);
            return StartOwned(source, destination, encoding, null);
        } catch {
            safeHandle.Dispose();
            throw;
        }
    }

    private sealed class GuardedReadStream : Stream {
        private readonly byte[] data;
        private readonly int bound;
        private int position;
        public int MaxReadRequest { get; private set; }

        public GuardedReadStream(byte[] data, int bound) {
            this.data = data;
            this.bound = bound;
        }

        public override int Read(byte[] buffer, int offset, int count) {
            if (count > bound)
                throw new InvalidOperationException("Copy requested more than its fixed buffer.");
            if (count > MaxReadRequest) MaxReadRequest = count;
            int available = data.Length - position;
            if (available == 0) return 0;
            int read = Math.Min(count, available);
            Array.Copy(data, position, buffer, offset, read);
            position += read;
            return read;
        }

        public override bool CanRead { get { return true; } }
        public override bool CanSeek { get { return false; } }
        public override bool CanWrite { get { return false; } }
        public override long Length { get { return data.Length; } }
        public override long Position {
            get { return position; }
            set { throw new NotSupportedException(); }
        }
        public override void Flush() { }
        public override long Seek(long offset, SeekOrigin origin) { throw new NotSupportedException(); }
        public override void SetLength(long value) { throw new NotSupportedException(); }
        public override void Write(byte[] buffer, int offset, int count) { throw new NotSupportedException(); }
    }

    private sealed class RecordingTextWriter : TextWriter {
        private readonly StringBuilder output = new StringBuilder();
        public bool IsDisposed { get; private set; }
        public override Encoding Encoding { get { return new UTF8Encoding(false); } }

        public override void Write(char[] buffer, int index, int count) {
            if (IsDisposed) throw new ObjectDisposedException("RecordingTextWriter");
            output.Append(buffer, index, count);
        }

        public override void Flush() {
            if (IsDisposed) throw new ObjectDisposedException("RecordingTextWriter");
        }

        public override string ToString() { return output.ToString(); }

        protected override void Dispose(bool disposing) {
            IsDisposed = true;
            base.Dispose(disposing);
        }
    }

    private sealed class ThrowingTextWriter : TextWriter {
        public bool IsDisposed { get; private set; }
        public override Encoding Encoding { get { return new UTF8Encoding(false); } }

        public override void Write(char[] buffer, int index, int count) {
            throw new InvalidOperationException("Output destination failure.");
        }

        public override void Flush() { }

        protected override void Dispose(bool disposing) {
            IsDisposed = true;
            base.Dispose(disposing);
        }
    }

    private static void WaitForSignal(WaitHandle signal, string name) {
        if (!signal.WaitOne(5000))
            throw new InvalidOperationException("Output pump lifecycle self-test timed out: " + name);
    }

    private static void WaitForTask(Task task, string name) {
        if (!task.Wait(5000))
            throw new InvalidOperationException("Output pump lifecycle self-test task timed out: " + name);
        task.GetAwaiter().GetResult();
    }

    private static void VerifyDisposeOrdering(bool settleFirst) {
        var settleSignal = new ManualResetEvent(false);
        var releaseSettle = new ManualResetEvent(false);
        var beforeSettleSignal = new ManualResetEvent(false);
        var releaseBeforeSettle = new ManualResetEvent(false);
        var disposeStarted = new ManualResetEvent(false);
        var disposeCompleted = new ManualResetEvent(false);
        var destination = new RecordingTextWriter();
        Pump pump = null;
        Task disposeTask = null;
        try {
            var hooks = new LifecycleHooks {
                BeforeSettle = settleFirst ? null : (Action)(() => {
                    beforeSettleSignal.Set();
                    WaitForSignal(releaseBeforeSettle, "before-settle release");
                }),
                Settled = settleFirst ? (Action)(() => {
                    settleSignal.Set();
                    WaitForSignal(releaseSettle, "settle release");
                }) : null,
                DisposeStarted = () => disposeStarted.Set(),
                DisposeCompleted = () => disposeCompleted.Set()
            };
            pump = StartLifecycleTest(new GuardedReadStream(new byte[0], BufferSize),
                destination, new UTF8Encoding(false), hooks);
            if (settleFirst) {
                WaitForSignal(settleSignal, "settle commit");
            } else {
                WaitForSignal(beforeSettleSignal, "settle attempt");
            }
            disposeTask = Task.Run(() => pump.Dispose());
            WaitForSignal(disposeStarted, "dispose commit");
            WaitForSignal(disposeCompleted, "dispose completion");
            if (settleFirst) releaseSettle.Set(); else releaseBeforeSettle.Set();
            WaitForTask(disposeTask, "dispose");
            if (!pump.Wait(5000))
                throw new InvalidOperationException("Output pump lifecycle self-test pump timed out.");
            pump.Dispose();
            pump.Dispose();
            destination.Write("tail");
            if (destination.IsDisposed || destination.ToString() != "tail")
                throw new InvalidOperationException("Output pump lifecycle self-test closed its destination.");
        } finally {
            releaseSettle.Set();
            releaseBeforeSettle.Set();
            if (disposeTask != null) {
                try { disposeTask.Wait(5000); } catch { }
            }
            if (pump != null) {
                try { pump.Dispose(); } catch { }
            }
            settleSignal.Dispose();
            releaseSettle.Dispose();
            beforeSettleSignal.Dispose();
            releaseBeforeSettle.Dispose();
            disposeStarted.Dispose();
            disposeCompleted.Dispose();
        }
    }

    public static void VerifyDisposeLifecycle() {
        VerifyDisposeOrdering(true);
        VerifyDisposeOrdering(false);
        var destination = new ThrowingTextWriter();
        var pump = Start(new GuardedReadStream(new byte[] { 0x78 }, BufferSize),
            destination, new UTF8Encoding(false));
        bool observed = false;
        try {
            pump.Wait();
        } catch (InvalidOperationException error) {
            if (error.Message != "Output destination failure.") throw;
            observed = true;
        }
        if (!observed) throw new InvalidOperationException("Output pump lifecycle self-test missed task failure.");
        for (int index = 0; index < 2; index++) {
            try { pump.Dispose(); } catch (InvalidOperationException error) {
                if (error.Message != "Output destination failure.") throw;
            }
        }
        if (destination.IsDisposed)
            throw new InvalidOperationException("Output pump lifecycle self-test closed a failed destination.");
    }

    public static void VerifyBoundedPump() {
        string expected = new string('x', BufferSize + 5);
        byte[] input = new UTF8Encoding(false).GetBytes(expected);
        var source = new GuardedReadStream(input, BufferSize);
        var destination = new RecordingTextWriter();
        var pump = Start(source, destination, new UTF8Encoding(false));
        try {
            pump.Wait();
        } finally {
            pump.Dispose();
        }
        destination.Write("tail");
        if (source.MaxReadRequest > BufferSize || destination.IsDisposed ||
                destination.ToString() != expected + "tail")
            throw new InvalidOperationException("Bounded output pump self-test did not drain to EOF.");
    }
}
'@

function Convert-MemoryLimit([string] $value) {
    if ($value -notmatch '^([0-9]+)([kKmMgG])$') {
        throw "Memory limit must be an integer followed by k, m, or g: $value"
    }
    $number = [UInt64] $Matches[1]
    switch ($Matches[2].ToLowerInvariant()) {
        "k" { return $number * 1KB }
        "m" { return $number * 1MB }
        "g" { return $number * 1GB }
    }
}

function Wait-OutputPumps($stdoutPump, $stderrPump, [int] $timeoutMilliseconds = -1,
    [bool] $allowIncomplete = $false) {
    $pumps = @(@($stdoutPump, $stderrPump) | Where-Object { $null -ne $_ })
    if ($pumps.Count -eq 0) { return }
    if ($timeoutMilliseconds -lt 0) {
        foreach ($pump in $pumps) { $pump.Wait() }
        return
    }
    $watch = [Diagnostics.Stopwatch]::StartNew()
    $incomplete = $false
    $failure = $null
    foreach ($pump in $pumps) {
        $remaining = $timeoutMilliseconds - [int]$watch.ElapsedMilliseconds
        if ($remaining -le 0) {
            $incomplete = $true
            continue
        }
        try {
            if (-not $pump.Wait($remaining)) { $incomplete = $true }
        } catch {
            if ($null -eq $failure) { $failure = $_ }
        }
    }
    if ($incomplete -or $null -ne $failure) {
        $initialFailure = $failure
        foreach ($pump in $pumps) {
            try { $pump.Dispose() } catch { }
        }
        foreach ($pump in $pumps) {
            try { [void]$pump.Wait(1000) } catch {
                if ($null -eq $failure) { $failure = $_ }
            }
        }
        if ($null -ne $initialFailure) { throw $initialFailure }
        if ($allowIncomplete) { return }
        if ($null -ne $failure) { throw $failure }
        throw "Output pump did not complete after the child process terminated"
    }
}

function Invoke-AtomicLaunchSelfTest {
    $tempPath = [IO.Path]::Combine([IO.Path]::GetTempPath(), "rush-run-capped-" + [Guid]::NewGuid().ToString("N"))
    [IO.Directory]::CreateDirectory($tempPath) | Out-Null
    $processHandle = [IntPtr]::Zero
    $threadHandle = [IntPtr]::Zero
    $stdoutReadHandle = [IntPtr]::Zero
    $stderrReadHandle = [IntPtr]::Zero
    $job = [IntPtr]::Zero
    $sentinel = [IntPtr]::Zero
    $assigned = $false
    try {
        $markerPath = [IO.Path]::Combine($tempPath, "marker.txt")
        $probePath = [IO.Path]::Combine($tempPath, "probe.ps1")
        $probeContent = @(
            'param([Int64]$SentinelHandle, [string]$SentinelPath, [string]$MarkerPath)'
            'Add-Type @"'
            'using System;'
            'using System.IO;'
            'using System.Runtime.InteropServices;'
            'using System.Text;'
            'public static class RushHandleProbe {'
            '    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]'
            '    private static extern uint GetFinalPathNameByHandle('
            '        IntPtr handle, StringBuilder path, uint length, uint flags);'
            '    public static bool IsExpectedFile(IntPtr handle, string expectedPath) {'
            '        var path = new StringBuilder(1024);'
            '        uint length = GetFinalPathNameByHandle(handle, path, (uint)path.Capacity, 0);'
            '        if (length == 0 || length >= path.Capacity) return false;'
            '        string actual = path.ToString();'
            '        if (actual.StartsWith("\\\\?\\", StringComparison.Ordinal))'
            '            actual = actual.Substring(4);'
            '        return string.Equals(Path.GetFullPath(actual), Path.GetFullPath(expectedPath),'
            '            StringComparison.OrdinalIgnoreCase);'
            '    }'
            '}'
            '"@'
            '$handle = [IntPtr]::new($SentinelHandle)'
            '$result = if ([RushHandleProbe]::IsExpectedFile($handle, $SentinelPath)) { "inherited" } else { "not-inherited" }'
            '[IO.File]::WriteAllText($MarkerPath, $result)'
        ) -join [Environment]::NewLine
        Set-Content -LiteralPath $probePath -Value $probeContent -Encoding UTF8
        $sentinelPath = [IO.Path]::Combine($tempPath, "sentinel.bin")
        $sentinel = [RushJobObject]::CreateInheritableSentinel($sentinelPath)
        $commandParts = @(
            "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
            "-File", $probePath, $sentinel.ToInt64(), $sentinelPath, $markerPath
        )
        $commandLine = [RushJobObject]::BuildCommandLine([string[]]$commandParts)
        $memoryBytes = Convert-MemoryLimit "256m"
        [RushJobObject]::CreateSuspended($commandLine, [ref]$processHandle, [ref]$threadHandle, [ref]$stdoutReadHandle, [ref]$stderrReadHandle)
        if (Test-Path -LiteralPath $markerPath) {
            throw "atomic launch self-test child ran before containment"
        }
        $job = [RushJobObject]::Create($memoryBytes)
        [RushJobObject]::Assign($job, $processHandle)
        $assigned = $true
        if (Test-Path -LiteralPath $markerPath) {
            throw "atomic launch self-test child ran before resume"
        }
        [RushJobObject]::Resume($threadHandle)
        if (-not [RushJobObject]::Wait($processHandle, 5000)) {
            throw "atomic launch self-test child did not exit"
        }
        if (-not (Test-Path -LiteralPath $markerPath)) {
            throw "atomic launch self-test marker was not written"
        }
        if ((Get-Content -Raw -LiteralPath $markerPath) -ne "not-inherited") {
            throw "atomic launch self-test inherited an unrelated handle"
        }
    } finally {
        if ($processHandle -ne [IntPtr]::Zero) {
            try {
                if ($assigned) {
                    [RushJobObject]::TerminateJob($job, 1)
                } else {
                    [RushJobObject]::Terminate($processHandle, 1)
                }
            } catch { }
            try { [RushJobObject]::Wait($processHandle, 5000) | Out-Null } catch { }
        }
        if ($job -ne [IntPtr]::Zero) { [RushJobObject]::Close($job) }
        if ($processHandle -ne [IntPtr]::Zero) { [RushJobObject]::Close($processHandle) }
        if ($threadHandle -ne [IntPtr]::Zero) { [RushJobObject]::Close($threadHandle) }
        if ($stdoutReadHandle -ne [IntPtr]::Zero) { [RushJobObject]::Close($stdoutReadHandle) }
        if ($stderrReadHandle -ne [IntPtr]::Zero) { [RushJobObject]::Close($stderrReadHandle) }
        if ($sentinel -ne [IntPtr]::Zero) { [RushJobObject]::Close($sentinel) }
        Remove-Item -LiteralPath $tempPath -Recurse -Force -ErrorAction SilentlyContinue
    }
}

function Invoke-CapturedSelfTestProcess([string[]] $arguments, [Text.Encoding] $encoding, [string] $powershellPath = "powershell.exe") {
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = $powershellPath
    $psi.Arguments = [RushJobObject]::BuildCommandLine($arguments)
    $psi.UseShellExecute = $false
    $psi.CreateNoWindow = $true
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    if ($null -eq $encoding) { $encoding = [Console]::OutputEncoding }
    $psi.StandardOutputEncoding = $encoding
    $psi.StandardErrorEncoding = $encoding
    $process = New-Object System.Diagnostics.Process
    $process.StartInfo = $psi
    $stdoutPath = [IO.Path]::Combine([IO.Path]::GetTempPath(), "rush-capture-" + [Guid]::NewGuid().ToString("N") + ".out")
    $stderrPath = [IO.Path]::Combine([IO.Path]::GetTempPath(), "rush-capture-" + [Guid]::NewGuid().ToString("N") + ".err")
    $stdoutFile = $null
    $stderrFile = $null
    $stdoutWriter = $null
    $stderrWriter = $null
    $stdoutPump = $null
    $stderrPump = $null
    $pumpsSettled = $false
    $processStarted = $false
    try {
        if (-not $process.Start()) { throw "self-test process did not start" }
        $processStarted = $true
        $stdoutFile = [IO.File]::Open($stdoutPath, [IO.FileMode]::Create,
            [IO.FileAccess]::Write, [IO.FileShare]::Read)
        $stderrFile = [IO.File]::Open($stderrPath, [IO.FileMode]::Create,
            [IO.FileAccess]::Write, [IO.FileShare]::Read)
        $stdoutWriter = [IO.StreamWriter]::new($stdoutFile,
            [RushOutputPump]::WithoutPreamble($encoding), [RushOutputPump]::BufferSize, $true)
        $stderrWriter = [IO.StreamWriter]::new($stderrFile,
            [RushOutputPump]::WithoutPreamble($encoding), [RushOutputPump]::BufferSize, $true)
        $stdoutPump = [RushOutputPump]::Start($process.StandardOutput.BaseStream,
            $stdoutWriter, $encoding)
        $stderrPump = [RushOutputPump]::Start($process.StandardError.BaseStream,
            $stderrWriter, $encoding)
        if (-not $process.WaitForExit(10000)) {
            $process.Kill()
            [void]$process.WaitForExit(5000)
            throw "self-test process timed out"
        }
        Wait-OutputPumps $stdoutPump $stderrPump
        $pumpsSettled = $true
        $stdoutWriter.Flush()
        $stderrWriter.Flush()
        $stdoutWriter.Dispose()
        $stdoutWriter = $null
        $stderrWriter.Dispose()
        $stderrWriter = $null
        $stdoutFile.Dispose()
        $stdoutFile = $null
        $stderrFile.Dispose()
        $stderrFile = $null
        [pscustomobject]@{
            ExitCode = $process.ExitCode
            Stdout = [IO.File]::ReadAllText($stdoutPath, $encoding)
            Stderr = [IO.File]::ReadAllText($stderrPath, $encoding)
        }
    } finally {
        if ($processStarted -and -not $process.HasExited) {
            try { $process.Kill() } catch { }
            try { [void]$process.WaitForExit(5000) } catch { }
        }
        if (($stdoutPump -ne $null -or $stderrPump -ne $null) -and -not $pumpsSettled) {
            try { Wait-OutputPumps $stdoutPump $stderrPump } catch { }
        }
        if ($stdoutWriter -ne $null) { $stdoutWriter.Dispose() }
        if ($stderrWriter -ne $null) { $stderrWriter.Dispose() }
        if ($stdoutFile -ne $null) { $stdoutFile.Dispose() }
        if ($stderrFile -ne $null) { $stderrFile.Dispose() }
        $process.Dispose()
        Remove-Item -LiteralPath $stdoutPath, $stderrPath -Force -ErrorAction SilentlyContinue
    }
}

function Get-TreeTimeoutCleanupDecision([bool] $identityMatched, [bool] $hasExited) {
    return $identityMatched -and -not $hasExited
}

function Invoke-TreeTimeoutSelfTest([string] $self, [string] $tempPath, [string] $helperPath, [string] $powershellPath) {
    $markerPath = [IO.Path]::Combine($tempPath, "delayed-marker.txt")
    $readyPath = [IO.Path]::Combine($tempPath, "descendant.ready")
    $pidPath = [IO.Path]::Combine($tempPath, "descendant.pid")
    $nonce = [Guid]::NewGuid().ToString("N")
    $parentReadyEventName = "Local\RushRunCappedParentReady-$nonce"
    $childReadyEventName = "Local\RushRunCappedChildReady-$nonce"
    $parentReadyEvent = [Threading.EventWaitHandle]::new(
        $false, [Threading.EventResetMode]::ManualReset, $parentReadyEventName)
    $childReadyEvent = [Threading.EventWaitHandle]::new(
        $false, [Threading.EventResetMode]::ManualReset, $childReadyEventName)
    try {
        $testedCommand = @($helperPath, "parent", $helperPath, $markerPath, $readyPath, $pidPath,
            $parentReadyEventName, $childReadyEventName, $nonce)
        & $powershellPath -NoProfile -ExecutionPolicy Bypass -File $self 256m 10 $testedCommand
        $wrapperExitCode = $LASTEXITCODE
        if ($wrapperExitCode -ne 124) {
            throw "tree-timeout self-test expected exit 124, got $wrapperExitCode"
        }
        if (-not $parentReadyEvent.WaitOne(0)) {
            throw "tree-timeout self-test did not observe the descendant readiness event"
        }
        if (-not (Test-Path -LiteralPath $pidPath)) {
            throw "tree-timeout self-test did not observe the descendant handshake before the outer deadline"
        }
        $handshake = [IO.File]::ReadAllText($pidPath).Trim().Split('|')
        if ($handshake.Count -ne 3 -or $handshake[2] -cne $nonce) {
            throw "tree-timeout self-test observed an invalid PID handshake"
        }
        $descendantPid = 0
        $descendantStartTicks = 0L
        if (-not [int]::TryParse($handshake[0], [ref]$descendantPid) -or
                -not [long]::TryParse($handshake[1], [ref]$descendantStartTicks)) {
            throw "tree-timeout self-test observed an invalid process identity"
        }
        $descendant = $null
        $identityMatched = $false
        $descendantHasExited = $true
        try {
            $descendant = [Diagnostics.Process]::GetProcessById($descendantPid)
            $descendantHasExited = $descendant.HasExited
            if (-not $descendantHasExited) {
                $observedStartTicks = $descendant.StartTime.ToUniversalTime().Ticks
                if ($observedStartTicks -eq $descendantStartTicks) {
                    $identityMatched = $true
                }
            }
        } catch {
            $identityMatched = $false
        } finally {
            if ($descendant -ne $null) {
                if (Get-TreeTimeoutCleanupDecision $identityMatched $descendantHasExited) {
                    try { $descendant.Kill(); $descendant.WaitForExit(5000) } catch { }
                }
                $descendant.Dispose()
            }
        }
        if ($identityMatched) {
            throw "tree-timeout self-test descendant survived the job termination"
        }
        if (Test-Path -LiteralPath $markerPath) {
            throw "tree-timeout self-test descendant wrote its delayed marker"
        }
    } finally {
        $childReadyEvent.Dispose()
        $parentReadyEvent.Dispose()
    }
}

if ($SelfTest) {
    [RushOutputPump]::VerifyDisposeLifecycle()
    [RushOutputPump]::VerifyBoundedPump()
    Invoke-AtomicLaunchSelfTest
    $self = $MyInvocation.MyCommand.Path
    $powershellPath = [IO.Path]::Combine($PSHOME, "powershell.exe")
    $selfTestTempPath = [IO.Path]::Combine([IO.Path]::GetTempPath(), "rush-run-capped-selftest-" + [Guid]::NewGuid().ToString("N"))
    [IO.Directory]::CreateDirectory($selfTestTempPath) | Out-Null
    $previousEncoding = [Console]::OutputEncoding
    try {
        [Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false)
        $argumentExecutable = [IO.Path]::Combine($selfTestTempPath, "arguments.exe")
        $argumentSource = @'
using System;
using System.Text;
public static class RushArgumentOracle {
    public static void Main(string[] args) {
        foreach (string arg in args)
            Console.WriteLine(Convert.ToBase64String(Encoding.UTF8.GetBytes(arg)));
    }
}
'@
        Add-Type -TypeDefinition $argumentSource -OutputAssembly $argumentExecutable -OutputType ConsoleApplication
        $argumentDriver = [IO.Path]::Combine($selfTestTempPath, "argument-driver.ps1")
        Set-Content -LiteralPath $argumentDriver -Encoding UTF8 -Value @'
param([string]$Self, [string]$Helper)
$previousEncoding = [Console]::OutputEncoding
[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false)
$values = @(
    "", "space value", "embedded`"quote", "trailing\",
    [string]::Concat([char]0x0416, [char]0x0430, [char]0x0440, "-",
        [char]0x043F, [char]0x0442, [char]0x0438, [char]0x0446, [char]0x0430,
        " ", [char]0xD83C, [char]0xDF0D)
)
& $Self 256m 30 $Helper $values[0] $values[1] $values[2] $values[3] $values[4]
[Console]::OutputEncoding = $previousEncoding
exit $LASTEXITCODE
'@
        $argumentValues = @(
            "", "space value", "embedded`"quote", "trailing\",
            [string]::Concat([char]0x0416, [char]0x0430, [char]0x0440, "-",
                [char]0x043F, [char]0x0442, [char]0x0438, [char]0x0446, [char]0x0430,
                " ", [char]0xD83C, [char]0xDF0D)
        )
        $argumentProcessArgs = @(
            "powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", $argumentDriver,
            $self, $argumentExecutable
        )
        $argumentResult = Invoke-CapturedSelfTestProcess $argumentProcessArgs
        if ($argumentResult.ExitCode -ne 0) {
            throw "argument self-test expected exit 0, got $($argumentResult.ExitCode): $($argumentResult.Stderr)"
        }
        if ($argumentResult.Stderr.Length -ne 0) {
            throw "argument self-test wrote unexpected stderr"
        }
        $encodedArguments = @($argumentResult.Stdout -split "`r?`n")
        if ($encodedArguments.Count -gt 0 -and $encodedArguments[-1] -eq "") {
            $encodedArguments = $encodedArguments[0..($encodedArguments.Count - 2)]
        }
        if ($encodedArguments.Count -ne $argumentValues.Count) {
            throw "argument self-test received $($encodedArguments.Count) arguments"
        }
        for ($index = 0; $index -lt $argumentValues.Count; $index++) {
            $received = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($encodedArguments[$index]))
            if ($received -cne $argumentValues[$index]) {
                throw "argument self-test mismatch at index $index"
            }
        }

        $encodingExecutable = [IO.Path]::Combine($selfTestTempPath, "encoding.exe")
        $encodingSource = @'
using System;
using System.IO;
using System.Text;
public static class RushEncodingOracle {
    private static void Write(Stream stream, Encoding encoding, string value) {
        byte[] preamble = encoding.GetPreamble();
        byte[] data = encoding.GetBytes(value);
        stream.Write(preamble, 0, preamble.Length);
        stream.Write(data, 0, data.Length);
        stream.Flush();
    }
    public static void Main() {
        Write(Console.OpenStandardOutput(), new UTF8Encoding(false), "\u00e9");
        Write(Console.OpenStandardError(), new UTF8Encoding(false), "\u00ef");
    }
}
'@
        Add-Type -TypeDefinition $encodingSource -OutputAssembly $encodingExecutable -OutputType ConsoleApplication
        $selfTestProcessEncoding = New-Object System.Text.UTF8Encoding($false)
        $outputDriver = [IO.Path]::Combine($selfTestTempPath, "output-driver.ps1")
        Set-Content -LiteralPath $outputDriver -Encoding UTF8 -Value @'
param([string]$Self, [string]$Executable)
[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false)
& $Self 256m 30 $Executable
exit $LASTEXITCODE
'@
        $unicodeResult = Invoke-CapturedSelfTestProcess @(
            $powershellPath, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", $outputDriver,
            $self, $encodingExecutable
        ) $selfTestProcessEncoding $powershellPath
        $expectedUnicodeStdout = [string][char]0x00E9
        $expectedUnicodeStderr = [string][char]0x00EF
        if ($unicodeResult.ExitCode -ne 0 -or $unicodeResult.Stdout -ne $expectedUnicodeStdout -or
                $unicodeResult.Stderr -ne $expectedUnicodeStderr) {
            throw "encoding self-test returned unexpected stdout/stderr: exit=$($unicodeResult.ExitCode), out=[$($unicodeResult.Stdout)], err=[$($unicodeResult.Stderr)]"
        }

        $utf16Executable = [IO.Path]::Combine($selfTestTempPath, "utf16-encoding.exe")
        $utf16Source = @'
using System;
using System.IO;
using System.Text;
public static class RushUtf16EncodingOracle {
    private static void Write(Stream stream, Encoding encoding, string value) {
        byte[] preamble = encoding.GetPreamble();
        byte[] data = encoding.GetBytes(value);
        stream.Write(preamble, 0, preamble.Length);
        stream.Write(data, 0, data.Length);
        stream.Flush();
    }
    public static void Main() {
        Write(Console.OpenStandardOutput(), new UnicodeEncoding(false, true), "utf16-\u0416");
        Write(Console.OpenStandardError(), new UnicodeEncoding(true, true), "utf16-\u754C");
    }
}
'@
        Add-Type -TypeDefinition $utf16Source -OutputAssembly $utf16Executable -OutputType ConsoleApplication
        $utf16Result = Invoke-CapturedSelfTestProcess @(
            $powershellPath, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", $outputDriver,
            $self, $utf16Executable
        ) $selfTestProcessEncoding $powershellPath
        $expectedUtf16Stdout = "utf16-" + [char]0x0416
        $expectedUtf16Stderr = "utf16-" + [char]0x754C
        if ($utf16Result.ExitCode -ne 0 -or $utf16Result.Stdout -ne $expectedUtf16Stdout -or
                $utf16Result.Stderr -ne $expectedUtf16Stderr) {
            throw "UTF-16 encoding self-test returned unexpected streams: exit=$($utf16Result.ExitCode)"
        }

        $interleavedExecutable = [IO.Path]::Combine($selfTestTempPath, "interleaved.exe")
        $interleavedSource = @'
using System;
using System.IO;
using System.Text;
public static class RushInterleavedOracle {
    public static void Main() {
        Stream stdout = Console.OpenStandardOutput();
        Stream stderr = Console.OpenStandardError();
        for (int index = 0; index < 32; index++) {
            byte[] output = Encoding.UTF8.GetBytes("out-" + index + "-Ж\n");
            byte[] error = Encoding.UTF8.GetBytes("err-" + index + "-界\n");
            stdout.Write(output, 0, output.Length);
            stdout.Flush();
            stderr.Write(error, 0, error.Length);
            stderr.Flush();
        }
    }
}
'@
        Add-Type -TypeDefinition $interleavedSource -OutputAssembly $interleavedExecutable -OutputType ConsoleApplication
        $interleavedResult = Invoke-CapturedSelfTestProcess @(
            $powershellPath, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", $outputDriver,
            $self, $interleavedExecutable
        ) $selfTestProcessEncoding $powershellPath
        $expectedInterleavedStdout = ((0..31 | ForEach-Object { "out-$($_)-Ж" }) -join "`n") + "`n"
        $expectedInterleavedStderr = ((0..31 | ForEach-Object { "err-$($_)-界" }) -join "`n") + "`n"
        if ($interleavedResult.ExitCode -ne 0 -or $interleavedResult.Stdout -ne $expectedInterleavedStdout -or
                $interleavedResult.Stderr -ne $expectedInterleavedStderr) {
            throw "interleaved output self-test returned unexpected streams: exit=$($interleavedResult.ExitCode)"
        }

        $missingExecutable = [IO.Path]::Combine($selfTestTempPath, "missing.exe")
        $createFailureResult = Invoke-CapturedSelfTestProcess @(
            $powershellPath, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", $outputDriver,
            $self, $missingExecutable
        ) $selfTestProcessEncoding $powershellPath
        if ($createFailureResult.ExitCode -eq 0 -or $createFailureResult.Stderr.Length -eq 0) {
            throw "create-failure self-test did not surface process creation failure"
        }

        [RushJobObject]::ValidateLayouts((Convert-MemoryLimit "256m"))
        $mismatchWouldKill = Get-TreeTimeoutCleanupDecision $false $false
        if ($mismatchWouldKill) {
            throw "tree-timeout identity mismatch cleanup oracle would terminate an unrelated process"
        }
        $treeHelperPath = [IO.Path]::Combine($selfTestTempPath, "tree-timeout-helper.exe")
        $treeHelperSource = @'
using System;
using System.Diagnostics;
using System.IO;
using System.Text;
using System.Threading;

public static class RushTreeTimeoutHelper {
    private static string Quote(string value) {
        var result = new StringBuilder();
        result.Append('"');
        int backslashes = 0;
        foreach (char character in value ?? string.Empty) {
            if (character == '\\') {
                backslashes++;
            } else if (character == '"') {
                result.Append('\\', backslashes * 2 + 1);
                result.Append('"');
                backslashes = 0;
            } else {
                result.Append('\\', backslashes);
                result.Append(character);
                backslashes = 0;
            }
        }
        result.Append('\\', backslashes * 2);
        result.Append('"');
        return result.ToString();
    }

    private static string BuildCommandLine(string[] arguments) {
        var result = new StringBuilder();
        for (int index = 0; index < arguments.Length; index++) {
            if (index != 0) result.Append(' ');
            result.Append(Quote(arguments[index]));
        }
        return result.ToString();
    }

    private static void WriteAtomically(string path, string value) {
        string temporary = path + "." + Guid.NewGuid().ToString("N") + ".tmp";
        File.WriteAllText(temporary, value, new UTF8Encoding(false));
        File.Move(temporary, path);
    }

    private static int RunChild(string[] args) {
        string markerPath = args[1];
        string readyPath = args[2];
        string childReadyEventName = args[3];
        using (var process = Process.GetCurrentProcess())
        using (var readyEvent = EventWaitHandle.OpenExisting(childReadyEventName)) {
            string record = process.Id + "|" +
                process.StartTime.ToUniversalTime().Ticks;
            WriteAtomically(readyPath, record);
            readyEvent.Set();
        }
        using (var delayedGate = new ManualResetEvent(false)) {
            delayedGate.WaitOne(30000);
        }
        File.WriteAllText(markerPath, "escaped", new UTF8Encoding(false));
        return 0;
    }

    private static int RunParent(string[] args) {
        string helperPath = args[1];
        string markerPath = args[2];
        string readyPath = args[3];
        string handshakePath = args[4];
        string parentReadyEventName = args[5];
        string childReadyEventName = args[6];
        string nonce = args[7];
        using (var parentReadyEvent = EventWaitHandle.OpenExisting(parentReadyEventName))
        using (var childReadyEvent = EventWaitHandle.OpenExisting(childReadyEventName)) {
            var childStartInfo = new ProcessStartInfo {
                FileName = helperPath,
                Arguments = BuildCommandLine(new[] {
                    "child", markerPath, readyPath, childReadyEventName
                }),
                UseShellExecute = false,
                CreateNoWindow = true
            };
            using (var child = Process.Start(childStartInfo)) {
                if (child == null) throw new InvalidOperationException("Child helper did not start.");
                childReadyEvent.WaitOne();
                string[] record = File.ReadAllText(readyPath).Trim().Split('|');
                if (record.Length != 2)
                    throw new InvalidDataException("Child helper readiness record was malformed.");
                int childPid;
                long childStartTicks;
                if (!int.TryParse(record[0], out childPid) ||
                        !long.TryParse(record[1], out childStartTicks) ||
                        childPid != child.Id ||
                        childStartTicks != child.StartTime.ToUniversalTime().Ticks) {
                    throw new InvalidDataException("Child helper readiness identity did not match its handle.");
                }
                WriteAtomically(handshakePath,
                    childPid + "|" + childStartTicks + "|" + nonce);
                parentReadyEvent.Set();
                child.WaitForExit();
                return child.ExitCode;
            }
        }
    }

    public static int Main(string[] args) {
        try {
            if (args.Length == 0) throw new ArgumentException("Helper mode is required.");
            if (args[0] == "child" && args.Length == 4) return RunChild(args);
            if (args[0] == "parent" && args.Length == 8) return RunParent(args);
            throw new ArgumentException("Helper arguments were malformed.");
        } catch (Exception error) {
            Console.Error.WriteLine(error.ToString());
            return 1;
        }
    }
}
'@
        Add-Type -TypeDefinition $treeHelperSource -OutputAssembly $treeHelperPath -OutputType ConsoleApplication
        Invoke-TreeTimeoutSelfTest $self $selfTestTempPath $treeHelperPath $powershellPath
    } finally {
        [Console]::OutputEncoding = $previousEncoding
        Remove-Item -LiteralPath $selfTestTempPath -Recurse -Force -ErrorAction SilentlyContinue
    }
    & $powershellPath -NoProfile -ExecutionPolicy Bypass -File $self 256m 1 $powershellPath -NoProfile -Command "Start-Sleep -Seconds 5"
    if ($LASTEXITCODE -ne 124) {
        throw "timeout self-test expected exit 124, got $LASTEXITCODE"
    }
    & $powershellPath -NoProfile -ExecutionPolicy Bypass -File $self 256m 30 $powershellPath -NoProfile -Command "exit 7"
    if ($LASTEXITCODE -ne 7) {
        throw "exit-code self-test expected exit 7, got $LASTEXITCODE"
    }
    if ($SelfTestArchitecture -eq "auto") {
        $otherArchitecture = if ([Environment]::Is64BitProcess) { "x86" } else { "x64" }
        $otherPowerShellPath = if ($otherArchitecture -eq "x86") {
            [IO.Path]::Combine($env:WINDIR, "SysWOW64", "WindowsPowerShell", "v1.0", "powershell.exe")
        } else {
            [IO.Path]::Combine($env:WINDIR, "Sysnative", "WindowsPowerShell", "v1.0", "powershell.exe")
        }
        if (Test-Path -LiteralPath $otherPowerShellPath) {
            & $otherPowerShellPath -NoProfile -ExecutionPolicy Bypass -File $self `
                -SelfTest -Architecture $otherArchitecture
            $otherExitCode = $LASTEXITCODE
            if ($otherExitCode -ne 0) {
                throw "$otherArchitecture self-test failed with exit $otherExitCode"
            }
            Write-Output "run_capped.ps1 self-test architecture $otherArchitecture passed"
        } else {
            if ([Environment]::Is64BitOperatingSystem) {
                throw "Self-test architecture $otherArchitecture is required on this 64-bit OS but PowerShell is missing: $otherPowerShellPath"
            }
            Write-Output "run_capped.ps1 self-test architecture x64 unavailable on a 32-bit OS: $otherPowerShellPath"
        }
    }
    $currentArchitecture = if ([Environment]::Is64BitProcess) { "x64" } else { "x86" }
    Write-Output "run_capped.ps1 self-test architecture $currentArchitecture passed"
    Write-Output "run_capped.ps1 self-test passed"
    exit 0
}

if ($TimeoutSeconds -le 0) { throw "Timeout must be positive" }
if ($null -eq $Command -or $Command.Count -eq 0) { throw "A command is required" }
$memoryBytes = Convert-MemoryLimit $MemoryLimit

        $commandLine = [RushJobObject]::BuildCommandLine([string[]]$Command)
$processHandle = [IntPtr]::Zero
$threadHandle = [IntPtr]::Zero
$stdoutReadHandle = [IntPtr]::Zero
$stderrReadHandle = [IntPtr]::Zero
$job = [IntPtr]::Zero
$stdoutPump = $null
$stderrPump = $null
$stdoutDestination = $null
$stderrDestination = $null
$stdoutRaw = $null
$stderrRaw = $null
$consoleEncoding = [Console]::OutputEncoding
$assigned = $false
$jobClosed = $false
$exitCode = $null
$pumpsSettled = $false

try {
    $job = [RushJobObject]::Create($memoryBytes)
    [RushJobObject]::CreateSuspended($commandLine, [ref]$processHandle, [ref]$threadHandle, [ref]$stdoutReadHandle, [ref]$stderrReadHandle)
    try {
        $stdoutRaw = [Console]::OpenStandardOutput()
        $stdoutDestination = [IO.StreamWriter]::new($stdoutRaw,
            [RushOutputPump]::WithoutPreamble($consoleEncoding), [RushOutputPump]::BufferSize, $true)
        $stdoutPump = [RushOutputPump]::Start($stdoutReadHandle, $stdoutDestination, $consoleEncoding)
    } finally {
        $stdoutReadHandle = [IntPtr]::Zero
    }
    try {
        $stderrRaw = [Console]::OpenStandardError()
        $stderrDestination = [IO.StreamWriter]::new($stderrRaw,
            [RushOutputPump]::WithoutPreamble($consoleEncoding), [RushOutputPump]::BufferSize, $true)
        $stderrPump = [RushOutputPump]::Start($stderrReadHandle, $stderrDestination, $consoleEncoding)
    } finally {
        $stderrReadHandle = [IntPtr]::Zero
    }
    [RushJobObject]::Assign($job, $processHandle)
    $assigned = $true
    [RushJobObject]::Resume($threadHandle)

    $completed = [RushJobObject]::Wait($processHandle, [uint32]($TimeoutSeconds * 1000))
    if (-not $completed) {
        [RushJobObject]::TerminateJob($job, 124)
        [void][RushJobObject]::Wait($processHandle, 5000)
        [RushJobObject]::Close($job)
        $job = [IntPtr]::Zero
        $jobClosed = $true
        $exitCode = 124
    } else {
        [RushJobObject]::Close($job)
        $job = [IntPtr]::Zero
        $jobClosed = $true
        $exitCode = [RushJobObject]::ExitCode($processHandle)
    }

    if ($exitCode -eq 124) {
        Wait-OutputPumps $stdoutPump $stderrPump 5000 $true
    } else {
        Wait-OutputPumps $stdoutPump $stderrPump
    }
    $pumpsSettled = $true
    exit $exitCode
} finally {
    if ($processHandle -ne [IntPtr]::Zero) {
        try {
            if ($assigned -and -not $jobClosed -and $job -ne [IntPtr]::Zero) {
                [RushJobObject]::TerminateJob($job, 1)
            } elseif (-not $assigned) {
                [RushJobObject]::Terminate($processHandle, 1)
            }
        } catch { }
        try { [RushJobObject]::Wait($processHandle, 5000) | Out-Null } catch { }
    }
    $pumpCleanupError = $null
    if (($stdoutPump -ne $null -or $stderrPump -ne $null) -and -not $pumpsSettled) {
        try {
            Wait-OutputPumps $stdoutPump $stderrPump 5000 ($exitCode -eq 124)
        } catch {
            $pumpCleanupError = $_
        }
    }
    if ($job -ne [IntPtr]::Zero) { [RushJobObject]::Close($job) }
    try { if ($stdoutPump -ne $null) { $stdoutPump.Dispose() } } catch {
        if ($null -eq $pumpCleanupError) { $pumpCleanupError = $_ }
    }
    try { if ($stderrPump -ne $null) { $stderrPump.Dispose() } } catch {
        if ($null -eq $pumpCleanupError) { $pumpCleanupError = $_ }
    }
    try { if ($stdoutDestination -ne $null) { $stdoutDestination.Dispose() } } catch {
        if ($null -eq $pumpCleanupError) { $pumpCleanupError = $_ }
    }
    try { if ($stderrDestination -ne $null) { $stderrDestination.Dispose() } } catch {
        if ($null -eq $pumpCleanupError) { $pumpCleanupError = $_ }
    }
    if ($stdoutReadHandle -ne [IntPtr]::Zero) { [RushJobObject]::Close($stdoutReadHandle) }
    if ($stderrReadHandle -ne [IntPtr]::Zero) { [RushJobObject]::Close($stderrReadHandle) }
    if ($processHandle -ne [IntPtr]::Zero) { [RushJobObject]::Close($processHandle) }
    if ($threadHandle -ne [IntPtr]::Zero) { [RushJobObject]::Close($threadHandle) }
    if ($null -ne $pumpCleanupError) { throw $pumpCleanupError }
}
