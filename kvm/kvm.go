package kvm

import (
	"errors"
	"syscall"
	"unsafe"
)

const (
	kvmGetAPIVersion       = 44544
	kvmCreateVM            = 44545
	kvmCreateVCPU          = 44609
	kvmRun                 = 44672
	kvmGetVCPUMMapSize     = 44548
	kvmGetSregs            = 0x8138ae83
	kvmSetSregs            = 0x4138ae84
	kvmGetRegs             = 0x8090ae81
	kvmSetRegs             = 0x4090ae82
	kvmSetUserMemoryRegion = 1075883590
	kvmSetTSSAddr          = 0xae47
	kvmSetIdentityMapAddr  = 0x4008AE48
	kvmCreateIRQChip       = 0xAE60
	kvmCreatePIT2          = 0x4040AE77
	kvmGetSupportedCPUID   = 0xC008AE05
	kvmSetCPUID2           = 0x4008AE90
	kvmIRQLine             = 0xc008ae67

	EXITUNKNOWN       = 0
	EXITEXCEPTION     = 1
	EXITIO            = 2
	EXITHYPERCALL     = 3
	EXITDEBUG         = 4
	EXITHLT           = 5
	EXITMMIO          = 6
	EXITIRQWINDOWOPEN = 7
	EXITSHUTDOWN      = 8
	EXITFAILENTRY     = 9
	EXITINTR          = 10
	EXITSETTPR        = 11
	EXITTPRACCESS     = 12
	EXITS390SIEIC     = 13
	EXITS390RESET     = 14
	EXITDCR           = 15
	EXITNMI           = 16
	EXITINTERNALERROR = 17

	EXITIOIN  = 0
	EXITIOOUT = 1

	numInterrupts   = 0x100
	CPUIDFeatures   = 0x40000001
	CPUIDSignature  = 0x40000000
	CPUIDFuncPerMon = 0x0A
)

var ErrorUnexpectedEXITReason = errors.New("unexpected kvm exit reason")

// 通用寄存器
type Regs struct {
	RAX    uint64
	RBX    uint64
	RCX    uint64
	RDX    uint64
	RSI    uint64
	RDI    uint64
	RSP    uint64
	RBP    uint64
	R8     uint64
	R9     uint64
	R10    uint64
	R11    uint64
	R12    uint64
	R13    uint64
	R14    uint64
	R15    uint64
	RIP    uint64
	RFLAGS uint64
}

// 特殊寄存器
type Sregs struct {
	CS              Segment
	DS              Segment
	ES              Segment
	FS              Segment
	GS              Segment
	SS              Segment
	TR              Segment
	LDT             Segment
	GDT             Descriptor
	IDT             Descriptor
	CR0             uint64
	CR2             uint64
	CR3             uint64
	CR4             uint64
	CR8             uint64
	EFER            uint64
	ApicBase        uint64
	InterruptBitmap [(numInterrupts + 63) / 64]uint64
}

// 寄存器类型
type Segment struct {
	Base     uint64
	Limit    uint32
	Selector uint16
	Typ      uint8
	Present  uint8
	DPL      uint8
	DB       uint8
	S        uint8
	L        uint8
	G        uint8
	AVL      uint8
	Unusable uint8
	_        uint8
}

type Descriptor struct {
	Base  uint64
	Limit uint16
	_     [3]uint16
}

// kvm 中 kvm_run 结构体
type RunData struct {
	RequestInterruptWindow     uint8
	ImmediateExit              uint8
	_                          [6]uint8
	ExitReason                 uint32
	ReadyForInterruptInjection uint8
	IfFlag                     uint8
	_                          [2]uint8
	CR8                        uint64
	ApicBase                   uint64
	Data                       [32]uint64
}

// 当 kvm 因为 IO 退出时，通过 IO 获取到
func (r *RunData) IO() (uint64, uint64, uint64, uint64, uint64) {
	direction := r.Data[0] & 0xFF
	size := (r.Data[0] >> 8) & 0xFF
	port := (r.Data[0] >> 16) & 0xFFFF
	count := (r.Data[0] >> 32) & 0xFFFFFFFF
	offset := r.Data[1]

	return direction, size, port, count, offset
}

type UserspaceMemoryRegion struct {
	Slot          uint32
	Flags         uint32
	GuestPhysAddr uint64
	MemorySize    uint64
	UserspaceAddr uint64
}

func (r *UserspaceMemoryRegion) SetMemLogDirtyPages() {
	r.Flags |= 1 << 0
}

func (r *UserspaceMemoryRegion) SetMemReadonly() {
	r.Flags |= 1 << 1
}

// ioctl 系统调用
func ioctl(fd, op, arg uintptr) (uintptr, error) {
	res, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL, fd, op, arg)
	if errno != 0 {
		return res, errno
	}

	return res, nil
}

// 获取 kvm 版本，一般是 12 代表稳定版本
func GetAPIVersion(kvmFd uintptr) (uintptr, error) {
	return ioctl(kvmFd, uintptr(kvmGetAPIVersion), uintptr(0))
}

// 创建 vm，调用命令 kvmCreateVM
func CreateVM(kvmFd uintptr) (uintptr, error) {
	return ioctl(kvmFd, uintptr(kvmCreateVM), uintptr(0))
}

// 在 vm 中创建 vcpu, 调用命令 kvmCreateVCPU
func CreateVCPU(vmFd uintptr, vcpuID int) (uintptr, error) {
	return ioctl(vmFd, uintptr(kvmCreateVCPU), uintptr(vcpuID))
}

// 运行 vpu，调用 kvmRun。这个运行在某时间点会退出
func Run(vcpuFd uintptr) error {
	_, err := ioctl(vcpuFd, uintptr(kvmRun), uintptr(0))
	if err != nil {
		// refs: https://github.com/kvmtool/kvmtool/blob/415f92c33a227c02f6719d4594af6fad10f07abf/kvm-cpu.c#L44
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR) {
			return nil
		}
	}

	return err
}

func GetVCPUMMmapSize(kvmFd uintptr) (uintptr, error) {
	return ioctl(kvmFd, uintptr(kvmGetVCPUMMapSize), uintptr(0))
}

// 获取特殊寄存器信息，调用命令 kvmGetSregs
func GetSregs(vcpuFd uintptr) (Sregs, error) {
	sregs := Sregs{}
	_, err := ioctl(vcpuFd, uintptr(kvmGetSregs), uintptr(unsafe.Pointer(&sregs)))

	return sregs, err
}

// 设置特殊寄存器信息，调用命令 kvmSetSregs
func SetSregs(vcpuFd uintptr, sregs Sregs) error {
	_, err := ioctl(vcpuFd, uintptr(kvmSetSregs), uintptr(unsafe.Pointer(&sregs)))

	return err
}

// 获取通用寄存器信息，调用命令 kvmGetRegs
func GetRegs(vcpuFd uintptr) (Regs, error) {
	regs := Regs{}
	_, err := ioctl(vcpuFd, uintptr(kvmGetRegs), uintptr(unsafe.Pointer(&regs)))

	return regs, err
}

// 设置通用寄存器信息，调用命令 kvmSetRegs
func SetRegs(vcpuFd uintptr, regs Regs) error {
	_, err := ioctl(vcpuFd, uintptr(kvmSetRegs), uintptr(unsafe.Pointer(&regs)))

	return err
}

// 对 vm 设置内存大小，调用命令 kvmSetUserMemoryRegion
func SetUserMemoryRegion(vmFd uintptr, region *UserspaceMemoryRegion) error {
	_, err := ioctl(vmFd, uintptr(kvmSetUserMemoryRegion), uintptr(unsafe.Pointer(region)))

	return err
}

// KVM_SET_TSS_ADDR
// 此 ioctl 定义来宾中三页区域的物理地址物理地址空间。该区域必须在该区域的前 4GB 范围内客户物理地址空间，
// 不得与任何内存插槽冲突或任何 mmio 地址。如果访客访问此内存，它可能会发生故障地区。基于 Intel 的主机需
// 要此 ioctl。这在 Intel 硬件上是必需的由于虚拟化实现中的一个怪癖（请参阅内部当它突然出现时的文档）。

// KVM_SET_TSS_ADDR: 會在客戶機物理內存起始位址分配 3 個頁面。猜測是用來存放 Task state segment (TSS)。
func SetTSSAddr(vmFd uintptr) error {
	_, err := ioctl(vmFd, kvmSetTSSAddr, 0xffffd000)

	return err
}

// 速懂X86虚拟化关键概念 - Intel EPT - 凌云萧萧的文章 - 知乎 https://zhuanlan.zhihu.com/p/41467047

// KVM_SET_IDENTITY_MAP_ADDR
// 此 ioctl 定义来宾中一页区域的物理地址物理地址空间。该区域必须在该区域的前 4GB 范围内客户物理地址空间，不得
// 与任何内存插槽冲突或任何 mmio 地址。如果访客访问此内存，它可能会发生故障地区。将地址设置为 0 将导致地址重置
// 为其默认值(0xfffbc000)。基于 Intel 的主机需要此 ioctl。这在 Intel 硬件上是必需的 由于虚拟化实现中的一个怪癖
//（请参阅内部当它突然出现时的文档）。
// KVM_SET_TSS_ADDR           Intel架构下初始化TSS内存区域
// KVM_SET_IDENTITY_MAP_ADDR  Intel架构下创建EPT真表

// EPT（Extended Page Tables，扩展页表），属于Intel的第二代硬件虚拟化技术，它是针对内存管理单元（MMU）的虚拟化扩展。
// 相对于影子页表，EPT降低了内存虚拟化的难度（，也提升了内存虚拟化的性能。从基于Intel的Nehalem架构的平台开始，EPT就作为
// CPU的一个特性加入到CPU硬件中去了。
// Intel在CPU中使用EPT技术，AMD也提供的类似技术叫做NPT，即Nested Page Tables。都是直接在硬件上支持GVA-->GPA-->HPA的两次
// 地址转换，从而降低内存虚拟化实现的复杂度，也进一步提升了内存虚拟化的性能
func SetIdentityMapAddr(vmFd uintptr) error {
	var mapAddr uint64 = 0xffffc000
	_, err := ioctl(vmFd, kvmSetIdentityMapAddr, uintptr(unsafe.Pointer(&mapAddr)))

	return err
}

type IRQLevel struct {
	IRQ   uint32
	Level uint32
}

// QEMU虚机关闭流程
// https://blog.csdn.net/huang987246510/article/details/103291419

// KVM_IOEVENTFD KVM_IRQFD
// https://www.cnblogs.com/dream397/p/14161550.html

// qemu模拟中断，主要是模拟处理中断的引脚和芯片，考虑一个外部设备作为中断源，从中断触发，到CPU中断引脚断言，再到CPU响应中断，这中间的过程如下：
// 1. 外部设备的输出引脚接中断处理芯片的输入引脚或直连到中断控制器的输入引脚，中断发生后，外部设备向它的上级设备提交中断信息。
// 2. 上级设备可以是普通芯片，也可以是中断控制器，如果是普通芯片，就继续迭代提交中断信息，直到中断信息到达中断控制器（Intel架构就是IO APIC，ARM结构就是GIC）。
// 3. 中断控制器的输入引脚断言到中断信息到达，会根据中断源的信息，判断这个中

// KVM_IRQ_LINE
// 将 GSI（GSI：Global System Interrupt） 输入的级别设置为内核中的中断控制器模型。在某些架构上，要求中断控制器模型具有之前是使用 KVM_CREATE_IRQCHIP 创建的。
// 注意边沿触发中断需要将级别设置为 1，然后再设置为 0。在真实硬件上，中断引脚可以是低电平有效或高电平有效。这对于 struct kvm_irq_level: 1 
// 的 level 字段无关紧要表示活动（断言），0 表示不活动（取消断言）。x86 允许操作系统编程中断极性（低电平有效/高电平有效）用于电平触发中断，并使用 KVM
// 考虑极性。但是，由于在处理低电平有效中断，上述约定现在在 x86 上也有效。这由 KVM_CAP_X86_IOAPIC_POLARITY_IGNORED 发出信号。
// 用户空间除非这存在能力（或者除非它没有使用内核中的 irqchip，当然）。

/*
触发1次中断

irq 中断编号，也就是中断引脚的编号
level 电平信息，边沿触发就是 0 或者 1

*/
func IRQLine(vmFd uintptr, irq, level uint32) error {
	irqLevel := IRQLevel{
		IRQ:   irq,
		Level: level,
	}

	_, err := ioctl(vmFd, kvmIRQLine, uintptr(unsafe.Pointer(&irqLevel)))

	return err
}

// 创建中断芯片，调用命令 kvmCreateIRQChip
func CreateIRQChip(vmFd uintptr) error {
	_, err := ioctl(vmFd, kvmCreateIRQChip, 0)

	return err
}

type PitConfig struct {
	Flags uint32
	_     [15]uint32
}

// KVM_CREATE_PIT2
// 为 i8254 PIT 创建内核设备模型。此调用仅有效通过 KVM_CREATE_IRQCHIP 启用内核内 irqchip 支持后。
// 一个操作系统要跑起来，必须有Time Tick，它就像是身体的脉搏。普通情况下，OS Time Tick由PIT(i8254)
// 或APIC Timer设备提供—PIT定期(1ms in Linux)产生一个timer interrupt，作为global tick, APIC Timer产生一个local tick。
// 在虚拟化情况下，必须为guest OS模拟一个PIT和APIC Timer。模拟的PIT和APIC Timer不能像真正硬件那样物理计时，所以一般用
// HOST的某种系统服务或软件计时器来为这个模拟PIT提供模拟”时钟源”
// https://royhunter.github.io/2015/11/20/interrupt-virtualization/
func CreatePIT2(vmFd uintptr) error {
	pit := PitConfig{
		Flags: 0,
	}
	_, err := ioctl(vmFd, kvmCreatePIT2, uintptr(unsafe.Pointer(&pit)))

	return err
}

type CPUID struct {
	Nent    uint32
	Padding uint32
	Entries [100]CPUIDEntry2
}

type CPUIDEntry2 struct {
	Function uint32
	Index    uint32
	Flags    uint32
	Eax      uint32
	Ebx      uint32
	Ecx      uint32
	Edx      uint32
	Padding  [3]uint32
}

// KVM_GET_SUPPORTED_CPUID
// 此 ioctl 返回 x86 cpuid 功能，两者均支持硬件和 kvm 的默认配置。用户空间可以使用
// 此 ioctl 返回的用于构造 cpuid 信息的信息（对于KVM_SET_CPUID2) 与硬件、内核和
// 用户空间功能，以及用户需求（例如，用户可能希望限制 cpuid 模拟旧硬件，或者整个集群的特征一致性）

// CPUID是Intel Pentium以上级CPU内置的一个指令(486级及以下的CPU不支持),它用于识别某一类型的CPU,它能返回CPU的级别(family),型号(model),CPU步进(Stepping ID)及CPU字串等信息,从此命令也可以得到CPU的缓存与TLB信息.
func GetSupportedCPUID(kvmFd uintptr, kvmCPUID *CPUID) error {
	_, err := ioctl(kvmFd, kvmGetSupportedCPUID, uintptr(unsafe.Pointer(kvmCPUID)))

	return err
}

func SetCPUID2(vcpuFd uintptr, kvmCPUID *CPUID) error {
	_, err := ioctl(vcpuFd, kvmSetCPUID2, uintptr(unsafe.Pointer(kvmCPUID)))

	return err
}
