// atomicwrite.go config.json 写盘的 bind mount 回退（panel 5e1422c9 + ab9a162b 移植）。
//
// 背景：config.json 以「目录」bind mount 进容器时，tmp+rename 原子替换正常工作；
// 但旧部署若仍以「单文件」bind mount（./config.json:/app/config.json），rename 的
// 目标是挂载点文件本身，内核挂载点检查直接以 EBUSY（"device or resource busy"）
// 拒绝——即使去掉 :ro 也一样。此时唯一可行路径是 open+truncate 原地写（放弃跨
// 文件原子性，换取单文件挂载形态下可落盘）。
//
// 两个调用方（admin patchConfigScalar 的 splice 落盘、cmd/server saveConfig 的整
// 文件重写）共用同一条回退链路，抽到这里避免两处分叉漂移。
package server

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// writeFileEBUSYFallback 把 tmp+rename 失败的写盘按 EBUSY 归因分流：
//   - renameErr 为 nil（rename 成功）：直接返回 nil，调用方继续走正常路径；
//   - renameErr 非 EBUSY：原样包装返回（权限、磁盘满等常规失败不适用原地写）；
//   - renameErr 为 EBUSY（单文件 bind mount 的挂载点拒绝 rename）：open+truncate 原地写。
//
// 原地写分支的失败语义（panel ab9a162b 加固）：
//   - open 失败：挂载文件尚未被破坏（O_TRUNC 未生效），tmp 可以清理，原文件仍是
//     完整旧内容；
//   - write/sync 失败：挂载文件已被 O_TRUNC 破坏，tmp 里是**唯一完整新内容**——
//     保留 tmp 不删，错误信息带 tmp 路径供手工恢复（mv tmp 覆盖回去即可）；
//   - 成功：清理 tmp。
//
// 注意：原地写绕过了 rename 的原子性，进程中断可能留下半截文件——这是单文件
// bind mount 形态下的固有取舍，docker-compose.yml 已注明目录挂载才是首选。
func WriteFileEBUSYFallback(renameErr error, tmp, path string, out []byte) error {
	if renameErr == nil {
		return nil
	}
	// 仅对 EBUSY（挂载点检查拒绝 rename）回退原地写；其他错误（权限/磁盘满等）
	// 原地写同样会失败，且会掩盖真实原因，原样返回。
	if !errors.Is(renameErr, syscall.EBUSY) {
		return fmt.Errorf("rename: %w", renameErr)
	}
	// 单文件 Docker bind mount 无法 rename 覆盖挂载点（Linux 返回 EBUSY），
	// 常规文件走原子替换，此部署形态退化为原地更新。
	f, openErr := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
	if openErr != nil {
		// open 失败时挂载文件未被截断，tmp 可安全清理。
		_ = os.Remove(tmp)
		return fmt.Errorf("rename: %w; bind mount fallback open: %w", renameErr, openErr)
	}
	_, writeErr := f.Write(out)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	// 写失败时保留 tmp（挂载文件已被 O_TRUNC 破坏，tmp 里是完整新内容，
	// 可手工恢复）；写成功才清理。
	if writeErr != nil {
		return fmt.Errorf("bind mount fallback write (完整新内容保留在 %s，可手工覆盖回 %s): %w", tmp, path, writeErr)
	}
	_ = os.Remove(tmp)
	if closeErr != nil {
		return fmt.Errorf("bind mount fallback close: %w", closeErr)
	}
	return nil
}
