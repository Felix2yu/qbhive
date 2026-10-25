package filemgr

import (
	"errors"
	"io"
	"os"
	"syscall"

	"github.com/Felix2yu/qbhive/internal/logger"
)

// moveFileSafe 把一个文件移动到目标路径，跨卷时自动退化为「复制 → 落盘 → 原子改名 → 删源」。
//
// 归档的落点常常与下载目录不在同一卷（下载盘 → 归档盘是常见布局），而 os.Rename
// 跨设备会直接返回 EXDEV，旧实现因此对这类用户完全不可用：qB renameFile 失败后
// 的本地回退必然失败，任务卡在原地反复重试。
//
// 目标已存在时直接报错，绝不覆盖——调用方在挑选目标名时已经做过冲突预检，
// 这里再兜一层防止并发或预检后状态变化导致的覆盖。
func moveFileSafe(src, dst string) error {
	if _, err := os.Lstat(dst); err == nil {
		return errors.New("目标已存在，拒绝覆盖：" + dst)
	}
	err := os.Rename(src, dst)
	if err == nil {
		return nil
	}
	if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	logger.Info.Printf("跨卷移动 %s → %s，退化为复制后删除", src, dst)
	return copyThenRemove(src, dst)
}

// copyThenRemove 复制 src 到 dst 并删除 src。
// 先写 dst 同目录下的临时文件、fsync 后再原子改名，确保任何时刻 dst 要么是完整文件、
// 要么不存在，不会留下半截文件被后续扫描当成有效产物。
func copyThenRemove(src, dst string) (retErr error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	si, err := in.Stat()
	if err != nil {
		return err
	}
	tmp := dst + ".qbhive-part"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, si.Mode().Perm())
	if err != nil {
		return err
	}
	// 出错时清掉半成品，避免留下 *-part 残留
	ok := false
	defer func() {
		out.Close()
		if !ok {
			_ = os.Remove(tmp)
		}
	}()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	ok = true
	if err := os.Remove(src); err != nil {
		// 复制已成功、源删不掉：目标文件是完整的，报错让调用方知道源还在即可
		return err
	}
	return nil
}
