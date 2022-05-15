package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"sync"

	"github.com/bobuhiro11/gokvm/flag"
	"github.com/bobuhiro11/gokvm/machine"
	"github.com/bobuhiro11/gokvm/term"
)

func main() {
	kernelPath, initrdPath, params, tapIfName, diskPath, nCpus, err := flag.ParseArgs(os.Args)
	if err != nil {
		log.Fatalf("ParseArgs: %v", err)
	}

	m, err := machine.New(nCpus, tapIfName, diskPath)
	if err != nil {
		log.Fatalf("%v", err)
	}

	kern, err := os.Open(kernelPath)
	if err != nil {
		log.Fatal(err)
	}

	initrd, err := os.Open(initrdPath)
	if err != nil {
		log.Fatal(err)
	}

	if err := m.LoadLinux(kern, initrd, params); err != nil {
		log.Fatalf("%v", err)
	}

	var wg sync.WaitGroup

	// 启动后台协程运行 cpu
	for i := 0; i < nCpus; i++ {
		fmt.Printf("Start CPU %d of %d\r\n", i, nCpus)
		wg.Add(1)

		go func(cpuId int) {
			// 运行 kvm 虚拟机线程
			if err = m.RunInfiniteLoop(cpuId); err != nil {
				fmt.Printf("%v\n\r", err)
			}

			wg.Done()
			fmt.Printf("CPU %d exits\n\r", cpuId)
		}(i)
	}

	if !term.IsTerminal() {
		fmt.Fprintln(os.Stderr, "this is not terminal and does not accept input")
		select {}
	}

	restoreMode, err := term.SetRawMode()
	if err != nil {
		log.Fatalf("%v", err)
	}

	defer restoreMode()

	var before byte = 0

	// 标准输入
	in := bufio.NewReader(os.Stdin)

	go func() {
		for {
			b, err := in.ReadByte() // 从标准输入中读取数据
			if err != nil {
				log.Printf("%v", err)

				break
			}
			m.GetInputChan() <- b // 从标准输入写入到串口的 input chan 中

			if len(m.GetInputChan()) > 0 { // 如果写入的数据大于 0，触发一次中断给内部的 linux 知道
				if err := m.InjectSerialIRQ(); err != nil {
					log.Printf("InjectSerialIRQ: %v", err)
				}
			}

			if before == 0x1 && b == 'x' { // 等于 x 时 退出当前 kvm
				restoreMode()
				os.Exit(0)
			}

			before = b
		}
	}()

	fmt.Printf("Waiting for CPUs to exit\r\n")

	// vcpu 协程运行完毕
	wg.Wait()
	fmt.Printf("All cpus done\n\r")
}
