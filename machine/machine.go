package machine

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"github.com/bobuhiro11/gokvm/bootparam"
	"github.com/bobuhiro11/gokvm/ebda"
	"github.com/bobuhiro11/gokvm/kvm"
	"github.com/bobuhiro11/gokvm/pci"
	"github.com/bobuhiro11/gokvm/serial"
	"github.com/bobuhiro11/gokvm/tap"
	"github.com/bobuhiro11/gokvm/virtio"
)

// InitialRegState GuestPhysAddr                      Binary files [+ offsets in the file]
//
//                 0x00000000    +------------------+
//                               |                  |
// RSI -->         0x00010000    +------------------+ bzImage [+ 0]
//                               |                  |
//                               |  boot param      |
//                               |                  |
//                               +------------------+
//                               |                  |
//                 0x00020000    +------------------+
//                               |                  |
//                               |   cmdline        |
//                               |                  |
//                               +------------------+
//                               |                  |
// RIP -->         0x00100000    +------------------+ bzImage [+ 512 x (setup_sects in boot param header + 1)]
//                               |                  |
//                               |   64bit kernel   |
//                               |                  |
//                               +------------------+
//                               |                  |
//                 0x0f000000    +------------------+ initrd [+ 0]
//                               |                  |
//                               |   initrd         |
//                               |                  |
//                               +------------------+
//                               |                  |
//                 0x40000000    +------------------+
const (
	memSize       = 1 << 30
	bootParamAddr = 0x10000
	cmdlineAddr   = 0x20000
	kernelAddr    = 0x100000
	initrdAddr    = 0xf000000

	// 硬件中断号
	serialIRQ    = 4
	// 换成 10 和 11 也可以工作，这里都是用的 pic（不是 apic）。https://www.webopedia.com/reference/irqnumbers/
	// 
	// (initramfs) cat /proc/interrupts 
	// 	   CPU0       CPU1       
	// 0:     437519          0    XT-PIC       timer
	// 2:          0          0    XT-PIC       cascade
	// 4:        199          1    XT-PIC       ttyS0
	// 9:          0          0    XT-PIC       virtio1
	// 10:          0          0    XT-PIC       virtio0
	// virtioNetIRQ = 9
	// virtioBlkIRQ = 10
	virtioNetIRQ = 10
	virtioBlkIRQ = 9
	
)

var (
	errorPCIDeviceNotFoundForPort = fmt.Errorf("pci device cannot be found for port")
	// ErrorWriteToCF9 indicates a write to cf9, the standard x86 reset port.
	ErrorWriteToCF9 = fmt.Errorf("power cycle via 0xcf9")
)

type Machine struct {
	kvmFd, vmFd    uintptr
	vcpuFds        []uintptr
	mem            []byte
	runs           []*kvm.RunData     // 加载的bzImage内核数据
	pci            *pci.PCI           // net 和 blk
	serial         *serial.Serial     // 串口
	ioportHandlers [0x10000][2]func(m *Machine, port uint64, bytes []byte) error
}

// 创建新虚拟机
func New(nCpus int, tapIfName string, diskPath string) (*Machine, error) {
	m := &Machine{}

	devKVM, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0o644)
	if err != nil {
		return m, fmt.Errorf(`/dev/kvm: %w`, err)
	}

	// kvm 虚拟机 fd
	m.kvmFd = devKVM.Fd()
	m.vcpuFds = make([]uintptr, nCpus)
	m.runs = make([]*kvm.RunData, nCpus)

	// 设置 kvm 内部的寄存器
	if m.vmFd, err = kvm.CreateVM(m.kvmFd); err != nil {
		return m, fmt.Errorf("CreateVM: %w", err)
	}

	// 设置 TSS 地址
	if err := kvm.SetTSSAddr(m.vmFd); err != nil {
		return m, err
	}

	if err := kvm.SetIdentityMapAddr(m.vmFd); err != nil {
		return m, err
	}

	// vm 创建中断芯片
	if err := kvm.CreateIRQChip(m.vmFd); err != nil {
		return m, err
	}

	// vm 创建时间设备
	if err := kvm.CreatePIT2(m.vmFd); err != nil {
		return m, err
	}

	// vm 获取 cpu 对应的内存大小
	mmapSize, err := kvm.GetVCPUMMmapSize(m.kvmFd)
	if err != nil {
		return m, err
	}

	// 根据传入的参数，创建 CPU
	for i := 0; i < nCpus; i++ {
		// Create vCPU
		m.vcpuFds[i], err = kvm.CreateVCPU(m.vmFd, i)
		if err != nil {
			return m, err
		}

		// init CPUID
		if err := m.initCPUID(i); err != nil {
			return m, err
		}

		// 获取到 kvm_run 结构体
		// init kvm_run structure
		r, err := syscall.Mmap(int(m.vcpuFds[i]), 0, int(mmapSize), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			return m, err
		}

		m.runs[i] = (*kvm.RunData)(unsafe.Pointer(&r[0]))
	}

	// 申请系统内存并设置
	m.mem, err = syscall.Mmap(-1, 0, memSize,
		syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED|syscall.MAP_ANONYMOUS)
	if err != nil {
		return m, err
	}

	err = kvm.SetUserMemoryRegion(m.vmFd, &kvm.UserspaceMemoryRegion{
		Slot: 0, Flags: 0, GuestPhysAddr: 0, MemorySize: 1 << 30,
		UserspaceAddr: uint64(uintptr(unsafe.Pointer(&m.mem[0]))),
	})
	if err != nil {
		return m, err
	}

	// Extended BIOS Data Area (EBDA).
	e, err := ebda.New(nCpus)
	if err != nil {
		return m, err
	}

	bytes, err := e.Bytes()
	if err != nil {
		return m, err
	}

	// EBDA的参数代码写入到参数位置
	copy(m.mem[bootparam.EBDAStart:], bytes)

	// 母机创建 tap 设备，给网络使用
	t, err := tap.New(tapIfName)
	if err != nil {
		return nil, err
	}

	// 创建 virtioNet，virtioBlk
	virtioNet := virtio.NewNet(virtioNetIRQ, m, t, m.mem)
	go virtioNet.TxThreadEntry()
	go virtioNet.RxThreadEntry()

	virtioBlk, err := virtio.NewBlk(diskPath, virtioBlkIRQ, m, m.mem)
	if err != nil {
		return nil, err
	}

	go virtioBlk.IOThreadEntry()

	// kvm 的 pci赋值
	m.pci = pci.New(
		pci.NewBridge(), // 00:00.0 for PCI bridge
		virtioNet,       // 00:01.0 for Virtio net
		virtioBlk,       // 00:02.0 for Virtio blk
	)

	return m, nil
}

// RunData returns the kvm.RunData for the VM.
func (m *Machine) RunData() []*kvm.RunData {
	return m.runs
}

// 通过 Linux Boot 协议加载 bzImage
func (m *Machine) LoadLinux(kernel, initrd io.ReaderAt, params string) error {
	// Load initrd
	initrdSize, err := initrd.ReadAt(m.mem[initrdAddr:], 0)
	if err != nil && initrdSize == 0 && !errors.Is(err, io.EOF) {
		return fmt.Errorf("initrd: (%v, %w)", initrdSize, err)
	}

	// Load kernel command-line parameters
	copy(m.mem[cmdlineAddr:], params)
	m.mem[cmdlineAddr+len(params)] = 0 // for null terminated string

	// Load Boot Param
	bootParam, err := bootparam.New(kernel)
	if err != nil {
		return err
	}

	// refs https://github.com/kvmtool/kvmtool/blob/0e1882a49f81cb15d328ef83a78849c0ea26eecc/x86/bios.c#L66-L86
	bootParam.AddE820Entry(
		bootparam.RealModeIvtBegin,
		bootparam.EBDAStart-bootparam.RealModeIvtBegin,
		bootparam.E820Ram,
	)
	bootParam.AddE820Entry(
		bootparam.EBDAStart,
		bootparam.VGARAMBegin-bootparam.EBDAStart,
		bootparam.E820Reserved,
	)
	bootParam.AddE820Entry(
		bootparam.MBBIOSBegin,
		bootparam.MBBIOSEnd-bootparam.MBBIOSBegin,
		bootparam.E820Reserved,
	)
	bootParam.AddE820Entry(
		kernelAddr,
		memSize-kernelAddr,
		bootparam.E820Ram,
	)

	bootParam.Hdr.VidMode = 0xFFFF                                                                  // Proto ALL
	bootParam.Hdr.TypeOfLoader = 0xFF                                                               // Proto 2.00+
	bootParam.Hdr.RamdiskImage = initrdAddr                                                         // Proto 2.00+
	bootParam.Hdr.RamdiskSize = uint32(initrdSize)                                                  // Proto 2.00+
	bootParam.Hdr.LoadFlags |= bootparam.CanUseHeap | bootparam.LoadedHigh | bootparam.KeepSegments // Proto 2.00+
	bootParam.Hdr.HeapEndPtr = 0xFE00                                                               // Proto 2.01+
	bootParam.Hdr.ExtLoaderVer = 0                                                                  // Proto 2.02+
	bootParam.Hdr.CmdlinePtr = cmdlineAddr                                                          // Proto 2.06+
	bootParam.Hdr.CmdlineSize = uint32(len(params) + 1)                                             // Proto 2.06+

	bytes, err := bootParam.Bytes()
	if err != nil {
		return err
	}

	copy(m.mem[bootParamAddr:], bytes)

	// Load kernel
	// copy to g.mem with offest setupsz
	//
	// The 32-bit (non-real-mode) kernel starts at offset (setup_sects+1)*512 in
	// the kernel file (again, if setup_sects == 0 the real value is 4.) It should
	// be loaded at address 0x10000 for Image/zImage kernels and 0x100000 for bzImage kernels.
	//
	// refs: https://www.kernel.org/doc/html/latest/x86/boot.html#loading-the-rest-of-the-kernel
	offset := int(bootParam.Hdr.SetupSects+1) * 512

	kernSize, err := kernel.ReadAt(m.mem[kernelAddr:], int64(offset))
	if err != nil && kernSize == 0 && !errors.Is(err, io.EOF) {
		return fmt.Errorf("kernel: (%v, %w)", kernSize, err)
	}

	for i := range m.vcpuFds {
		if err = m.initRegs(i); err != nil {
			return err
		}

		if err = m.initSregs(i); err != nil {
			return err
		}
	}

	m.initIOPortHandlers()

	// 创建串口并且赋值给 m.serial
	if m.serial, err = serial.New(m); err != nil {
		return err
	}

	return nil
}

func (m *Machine) GetInputChan() chan<- byte {
	return m.serial.GetInputChan()
}

// 初始化通用寄出器
func (m *Machine) initRegs(i int) error {
	regs, err := kvm.GetRegs(m.vcpuFds[i])
	if err != nil {
		return err
	}

	regs.RFLAGS = 2
	regs.RIP = kernelAddr
	regs.RSI = bootParamAddr

	if err := kvm.SetRegs(m.vcpuFds[i], regs); err != nil {
		return err
	}

	return nil
}

func (m *Machine) initSregs(i int) error {
	sregs, err := kvm.GetSregs(m.vcpuFds[i])
	if err != nil {
		return err
	}

	// set all segment flat
	sregs.CS.Base, sregs.CS.Limit, sregs.CS.G = 0, 0xFFFFFFFF, 1
	sregs.DS.Base, sregs.DS.Limit, sregs.DS.G = 0, 0xFFFFFFFF, 1
	sregs.FS.Base, sregs.FS.Limit, sregs.FS.G = 0, 0xFFFFFFFF, 1
	sregs.GS.Base, sregs.GS.Limit, sregs.GS.G = 0, 0xFFFFFFFF, 1
	sregs.ES.Base, sregs.ES.Limit, sregs.ES.G = 0, 0xFFFFFFFF, 1
	sregs.SS.Base, sregs.SS.Limit, sregs.SS.G = 0, 0xFFFFFFFF, 1

	sregs.CS.DB, sregs.SS.DB = 1, 1
	sregs.CR0 |= 1 // protected mode

	if err := kvm.SetSregs(m.vcpuFds[i], sregs); err != nil {
		return err
	}

	return nil
}

func (m *Machine) initCPUID(i int) error {
	cpuid := kvm.CPUID{}
	cpuid.Nent = 100

	if err := kvm.GetSupportedCPUID(m.kvmFd, &cpuid); err != nil {
		return err
	}

	// https://www.kernel.org/doc/html/latest/virt/kvm/cpuid.html
	for i := 0; i < int(cpuid.Nent); i++ {
		if cpuid.Entries[i].Function == kvm.CPUIDFuncPerMon {
			cpuid.Entries[i].Eax = 0 // disable
		} else if cpuid.Entries[i].Function == kvm.CPUIDSignature {
			cpuid.Entries[i].Eax = kvm.CPUIDFeatures
			cpuid.Entries[i].Ebx = 0x4b4d564b // KVMK
			cpuid.Entries[i].Ecx = 0x564b4d56 // VMKV
			cpuid.Entries[i].Edx = 0x4d       // M
		}
	}

	if err := kvm.SetCPUID2(m.vcpuFds[i], &cpuid); err != nil {
		return err
	}

	return nil
}

// vcpu 的运行循环
func (m *Machine) RunInfiniteLoop(i int) error {
	// https://www.kernel.org/doc/Documentation/virtual/kvm/api.txt
	// - vcpu ioctls: These query and set attributes that control the operation
	//   of a single virtual cpu.
	//
	//   vcpu ioctls should be issued from the same thread that was used to create
	//   the vcpu, except for asynchronous vcpu ioctl that are marked as such in
	//   the documentation.  Otherwise, the first ioctl after switching threads
	//   could see a performance impact.
	//
	// - device ioctls: These query and set attributes that control the operation
	//   of a single device.
	//
	//   device ioctls must be issued from the same process (address space) that
	//   was used to create the VM.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	for {
		isContinue, err := m.RunOnce(i)
		if err != nil {
			return err
		}

		if !isContinue {
			return nil
		}
	}
}

// 执行 1 次 cpu, 直到因为IO、中断等退出
func (m *Machine) RunOnce(i int) (bool, error) {
	err := kvm.Run(m.vcpuFds[i])

	switch m.runs[i].ExitReason {
	case kvm.EXITHLT:
		fmt.Println("KVM_EXIT_HLT")

		return false, err
	case kvm.EXITIO:
		direction, size, port, count, offset := m.runs[i].IO() // direction 代表方向有 EXITIOOUT 和 EXITIOIN
		f := m.ioportHandlers[port][direction]

		// 传入给 handler 的 bytes
		
		bytes := (*(*[100]byte)(unsafe.Pointer(uintptr(unsafe.Pointer(m.runs[i])) + uintptr(offset))))[0:size]

		for i := 0; i < int(count); i++ {
			if err := f(m, port, bytes); err != nil {
				return false, err
			}
		}

		return true, err
	case kvm.EXITUNKNOWN:
		return true, err
	case kvm.EXITINTR:
		// When a signal is sent to the thread hosting the VM it will result in EINTR
		// refs https://gist.github.com/mcastelino/df7e65ade874f6890f618dc51778d83a
		return true, nil
	default:
		if err != nil {
			return false, err
		}

		return false, fmt.Errorf("%w: %d", kvm.ErrorUnexpectedEXITReason, m.runs[i].ExitReason)
	}
}

// X86 将外设的寄存器看成一个独立的地址空间，所以访问内存的指令不能用来访问这些寄存器，
// 而要为对外设寄存器的读／写设置专用指令，如IN和OUT指令。这就是所谓的“ I/O端口”方式。

// 对于x86架构来说，通过IN/OUT指令访问。PC架构一共有65536个8bit的I/O端口，组成64K个
// I/O地址空间，编号从 0~0xFFFF，有16位，80x86用低16位地址线A0-A15来寻址。

// 初始化 IO 处理。当内核往设备中写入数据时，需要调用这些 handler。此时触发发kvm退出，退出原因 KVM_EXIT_IO
func (m *Machine) initIOPortHandlers() {
	funcNone := func(m *Machine, port uint64, bytes []byte) error {
		return nil
	}

	funcError := func(m *Machine, port uint64, bytes []byte) error {
		return fmt.Errorf("%w: unexpected io port 0x%x", kvm.ErrorUnexpectedEXITReason, port)
	}

	// 0xCF9 port can get three values for three types of reset:
	//
	// Writing 4 to 0xCF9:(INIT) Will INIT the CPU. Meaning it will jump
	// to the initial location of booting but it will keep many CPU
	// elements untouched. Most internal tables, chaches etc will remain
	// unchanged by the Init call (but may change during it).
	//
	// Writing 6 to 0xCF9:(RESET) Will RESET the CPU with all
	// internal tables caches etc cleared to initial state.
	//
	// Writing 0xE to 0xCF9:(RESTART) Will power cycle the mother board
	// with everything that comes with it.
	// For now, we will exit without regard to the value. Should we wish
	// to have more sophisticated cf9 handling, we will need to modify
	// gokvm a bit more.

	funcOutbCF9 := func(m *Machine, port uint64, bytes []byte) error {
		if len(bytes) == 1 && bytes[0] == 0xe {
			return fmt.Errorf("write 0xe to cf9: %w", ErrorWriteToCF9)
		}

		return fmt.Errorf("write %#x to cf9: %w", bytes, ErrorWriteToCF9)
	}

	// 设置默认的 IO 处理函数。从 0 到 0x10000 的port都被设置成默认处理函数
	// default handler
	for port := 0; port < 0x10000; port++ {
		for dir := kvm.EXITIOIN; dir <= kvm.EXITIOOUT; dir++ {
			m.ioportHandlers[port][dir] = funcError
		}
	}

	// 覆盖一些默认 handler
	for dir := kvm.EXITIOIN; dir <= kvm.EXITIOOUT; dir++ {
		// CF9
		m.ioportHandlers[0xcf9][dir] = funcNone
		m.ioportHandlers[0xcf9][dir] = funcOutbCF9

		// VGA
		for port := 0x3c0; port <= 0x3da; port++ {
			m.ioportHandlers[port][dir] = funcNone
		}

		for port := 0x3b4; port <= 0x3b5; port++ {
			m.ioportHandlers[port][dir] = funcNone
		}

		// CMOS clock
		for port := 0x70; port <= 0x71; port++ {
			m.ioportHandlers[port][dir] = funcNone
		}

		// DMA Page Registers (Commonly 74L612 Chip)
		for port := 0x80; port <= 0x9f; port++ {
			m.ioportHandlers[port][dir] = funcNone
		}

		// Serial port 2
		for port := 0x2f8; port <= 0x2ff; port++ {
			m.ioportHandlers[port][dir] = funcNone
		}

		// Serial port 3
		for port := 0x3e8; port <= 0x3ef; port++ {
			m.ioportHandlers[port][dir] = funcNone
		}

		// Serial port 4
		for port := 0x2e8; port <= 0x2ef; port++ {
			m.ioportHandlers[port][dir] = funcNone
		}

		// unknown
		for port := 0xcfe; port <= 0xcfe; port++ {
			m.ioportHandlers[port][dir] = funcNone
		}

		for port := 0xcfa; port <= 0xcfb; port++ {
			m.ioportHandlers[port][dir] = funcNone
		}

		// PCI Configuration Space Access Mechanism #2
		for port := 0xc000; port <= 0xcfff; port++ {
			m.ioportHandlers[port][dir] = funcNone
		}
	}

	// PS/2 Keyboard (Always 8042 Chip)
	for port := 0x60; port <= 0x6f; port++ {
		m.ioportHandlers[port][kvm.EXITIOIN] = func(m *Machine, port uint64, bytes []byte) error {
			// In ubuntu 20.04 on wsl2, the output to IO port 0x64 continued
			// infinitely. To deal with this issue, refer to kvmtool and
			// configure the input to the Status Register of the PS2 controller.
			//
			// refs:
			// https://github.com/kvmtool/kvmtool/blob/0e1882a49f81cb15d328ef83a78849c0ea26eecc/hw/i8042.c#L312
			// https://git.kernel.org/pub/scm/linux/kernel/git/will/kvmtool.git/tree/hw/i8042.c#n312
			// https://wiki.osdev.org/%228042%22_PS/2_Controller
			bytes[0] = 0x20

			return nil
		}
		m.ioportHandlers[port][kvm.EXITIOOUT] = funcNone
	}

	// 串口的读写。当 vm 退出时，要从设备读取数据
	// Serial port 1
	for port := serial.COM1Addr; port < serial.COM1Addr+8; port++ {
		m.ioportHandlers[port][kvm.EXITIOIN] = func(m *Machine, port uint64, bytes []byte) error {
			return m.serial.In(port, bytes) // 从设备读取数据
		}
		m.ioportHandlers[port][kvm.EXITIOOUT] = func(m *Machine, port uint64, bytes []byte) error {
			return m.serial.Out(port, bytes) // 往设备写入数据
		}
	}

	// PCI configuration
	//
	// 0xcf8 for address register for PCI Config Space
	// 0xcfc + 0xcff for data for PCI Config Space
	// see https://github.com/torvalds/linux/blob/master/arch/x86/pci/direct.c for more detail.

	m.ioportHandlers[0xCF8][kvm.EXITIOIN] = func(m *Machine, port uint64, bytes []byte) error {
		return m.pci.PciConfAddrIn(port, bytes)
	}
	m.ioportHandlers[0xCF8][kvm.EXITIOOUT] = func(m *Machine, port uint64, bytes []byte) error {
		return m.pci.PciConfAddrOut(port, bytes)
	}

	for port := 0xcfc; port < 0xcfc+4; port++ {
		m.ioportHandlers[port][kvm.EXITIOIN] = func(m *Machine, port uint64, bytes []byte) error {
			return m.pci.PciConfDataIn(port, bytes)
		}
		m.ioportHandlers[port][kvm.EXITIOOUT] = func(m *Machine, port uint64, bytes []byte) error {
			return m.pci.PciConfDataOut(port, bytes)
		}
	}

	// PCI devices
	for _, device := range m.pci.Devices {
		start, end := device.GetIORange()
		for port := start; port < end; port++ {
			m.ioportHandlers[port][kvm.EXITIOIN] = pciInFunc
			m.ioportHandlers[port][kvm.EXITIOOUT] = pciOutFunc
		}
	}
}

// EXITIOIN 退出引起。IO端口中读取数据，对应x86指令 IN
func pciInFunc(m *Machine, port uint64, bytes []byte) error {
	for i := range m.pci.Devices {
		start, end := m.pci.Devices[i].GetIORange()
		if start <= port && port < end {
			return m.pci.Devices[i].IOInHandler(port, bytes)
		}
	}

	return errorPCIDeviceNotFoundForPort
}

// EXITIOOUT 退出引起。IO端口中写入数据，对应x86指令 out
func pciOutFunc(m *Machine, port uint64, bytes []byte) error {
	for i := range m.pci.Devices {
		start, end := m.pci.Devices[i].GetIORange()
		if start <= port && port < end {
			return m.pci.Devices[i].IOOutHandler(port, bytes)
		}
	}

	return errorPCIDeviceNotFoundForPort
}

// 注入串口中断。也就是说触发一次串口中断
func (m *Machine) InjectSerialIRQ() error {
	if err := kvm.IRQLine(m.vmFd, serialIRQ, 0); err != nil {
		return err
	}

	if err := kvm.IRQLine(m.vmFd, serialIRQ, 1); err != nil {
		return err
	}

	return nil
}

// 注入网络中断。也就是说触发一次网络中断
func (m *Machine) InjectVirtioNetIRQ() error {
	if err := kvm.IRQLine(m.vmFd, virtioNetIRQ, 0); err != nil {
		return err
	}

	if err := kvm.IRQLine(m.vmFd, virtioNetIRQ, 1); err != nil {
		return err
	}

	return nil
}

// 注入磁盘中断。也就是说触发一次磁盘中断
func (m *Machine) InjectVirtioBlkIRQ() error {
	if err := kvm.IRQLine(m.vmFd, virtioBlkIRQ, 0); err != nil {
		return err
	}

	if err := kvm.IRQLine(m.vmFd, virtioBlkIRQ, 1); err != nil {
		return err
	}

	return nil
}
