package bootstrap

import (
	"debug/elf"
	"debug/macho"
	"errors"
	"fmt"
	"os"
	"strings"
)

func checkBinaryPlatform(binaryPath string, want Platform) error {
	info, err := os.Stat(binaryPath)
	if err != nil {
		return diagnostic(DiagnosticWrongArch, fmt.Errorf("inspect Mesh binary %s: %w", binaryPath, err))
	}
	if !info.Mode().IsRegular() {
		return diagnostic(DiagnosticWrongArch, fmt.Errorf("Mesh binary %s is not a regular file", binaryPath))
	}
	if info.Mode().Perm()&0o111 == 0 {
		return diagnostic(DiagnosticWrongArch, fmt.Errorf("Mesh binary %s is not executable", binaryPath))
	}
	platforms, err := binaryPlatforms(binaryPath)
	if err != nil {
		return diagnostic(DiagnosticWrongArch, err)
	}
	for _, platform := range platforms {
		if platform == want {
			return nil
		}
	}
	available := make([]string, len(platforms))
	for i, platform := range platforms {
		available[i] = platform.OS.String() + "/" + platform.Arch.String()
	}
	return diagnostic(DiagnosticWrongArch, fmt.Errorf("remote host needs %s/%s, but %s contains %s", want.OS, want.Arch, binaryPath, strings.Join(available, ", ")))
}

func binaryPlatforms(binaryPath string) ([]Platform, error) {
	elfFile, elfErr := elf.Open(binaryPath)
	if elfErr == nil {
		defer elfFile.Close() //nolint:errcheck // inspection result takes precedence over read-only cleanup
		if elfFile.OSABI != elf.ELFOSABI_NONE && elfFile.OSABI != elf.ELFOSABI_LINUX {
			return nil, fmt.Errorf("unsupported ELF operating-system ABI %s", elfFile.OSABI)
		}
		arch, err := archForELF(elfFile.Machine)
		if err != nil {
			return nil, err
		}
		return []Platform{{OS: Linux, Arch: arch}}, nil
	}

	machFile, machErr := macho.Open(binaryPath)
	if machErr == nil {
		defer machFile.Close() //nolint:errcheck // inspection result takes precedence over read-only cleanup
		arch, err := archForMachO(machFile.Cpu)
		if err != nil {
			return nil, err
		}
		return []Platform{{OS: Darwin, Arch: arch}}, nil
	}

	fatFile, fatErr := macho.OpenFat(binaryPath)
	if fatErr == nil {
		defer fatFile.Close() //nolint:errcheck // inspection result takes precedence over read-only cleanup
		platforms := make([]Platform, 0, len(fatFile.Arches))
		for _, file := range fatFile.Arches {
			arch, err := archForMachO(file.Cpu)
			if err != nil {
				return nil, err
			}
			platforms = append(platforms, Platform{OS: Darwin, Arch: arch})
		}
		return platforms, nil
	}
	return nil, fmt.Errorf("Mesh binary %s is neither ELF nor Mach-O: %w", binaryPath, errors.Join(elfErr, machErr, fatErr))
}

func archForELF(machine elf.Machine) (Arch, error) {
	switch machine {
	case elf.EM_X86_64:
		return AMD64, nil
	case elf.EM_AARCH64:
		return ARM64, nil
	default:
		return "", fmt.Errorf("unsupported ELF architecture %s", machine)
	}
}

func archForMachO(cpu macho.Cpu) (Arch, error) {
	switch cpu {
	case macho.CpuAmd64:
		return AMD64, nil
	case macho.CpuArm64:
		return ARM64, nil
	default:
		return "", fmt.Errorf("unsupported Mach-O architecture %s", cpu)
	}
}
