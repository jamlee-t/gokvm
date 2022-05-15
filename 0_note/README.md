aghosn/kvm.go
https://gist.github.com/aghosn/f72c8e8f53bf99c3c4117f49677ab0b9

rust-vmm
https://github.com/rust-vmm/kvm-bindings

硬件原理及固件开发
https://www.zhihu.com/column/c_1309501461563023360

https://www.binss.me/blog/qemu-note-of-interrupt/

计算机中断体系一：历史和原理 - 老狼的文章 - 知乎
https://zhuanlan.zhihu.com/p/26464793 👍👍👍👍👍

KVM中断控制器模拟
https://blog.csdn.net/huang987246510/article/details/103316327

KVM中断注入机制
https://blog.csdn.net/huang987246510/article/details/103397763

https://github.com/cc272309126/notes/blob/master/virtualization/qemu-kvm-interrupt.txt

Linux虚拟化KVM-Qemu分析（六）之中断虚拟化 ARM
https://www.cnblogs.com/LoyenWang/p/14017052.html 👍👍👍👍
https://www.cnblogs.com/LoyenWang/p/13052677.html
 
QEMU学习笔记——中断
https://www.binss.me/blog/qemu-note-of-interrupt/ 👍👍👍

设备的设备号分配
https://github.com/torvalds/linux/blob/master/Documentation/admin-guide/devices.txt

中断虚拟化起始关键在于对中断控制器的虚拟化，中断控制器目前主要有APIC，这种架构下设备控制器通过某种触发方式通知IO APIC，IO APIC根据自身维护的重定向表pci irq routing table格式化出一条中断消息，把中断消息发送给local APIC，local APIC局部与CPU，即每个CPU一个，local APIC 具备传统中断控制器的相关功能以及各个寄存器，中断请求寄存器IRR，中断屏蔽寄存器IMR，中断服务寄存器ISR等，针对这些关键部件的虚拟化是中断虚拟化的重点。在KVM架构下，每个KVM虚拟机维护一个Io APIC，但是每个VCPU有一个local APIC。

断信号可分为两类：硬件中断和软件中断，软件中断一般被称为异常。Intel x86有256个中断，每个中断都有一个0～255之间的数来表示，Intel将前32个中断号（0～31）已经固定设定好或者保留未用。中断号32～255分配给操作系统和应用程序使用。在Linux中，中断号32～47对应于一个硬件芯片的16个中断请求信号，这16个中断包括时钟、键盘、软盘、数学协处理器、硬盘等硬件的中断。系统调用设为中断号128，即0x80。