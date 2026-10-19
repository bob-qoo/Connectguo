import argparse
import os
import plistlib
import shutil
import subprocess
import sys
from pathlib import Path

from build_mirrors import china_mirror_environment, mirrored_pub_lockfile
from app_build import BuildVariant, add_variant_argument

root = Path(__file__).resolve().parents[1]

parser = argparse.ArgumentParser(description='构建红果鉴 / 真果鉴 macOS 应用')
parser.add_argument('--cn-mirrors', action='store_true', help='使用 Flutter 中国镜像')
add_variant_argument(parser)
options = parser.parse_args()
variant = BuildVariant(options.all_sources)

environment = os.environ.copy()
environment.setdefault('GOPROXY', 'https://goproxy.cn,direct')
environment.setdefault('GOSUMDB', 'off')

flutter = shutil.which('flutter')
if not flutter:
    raise SystemExit('请先将 Flutter SDK 的 bin 目录加入 PATH。')


def run(arguments, **kwargs):
    subprocess.run(arguments, cwd=kwargs.pop('cwd', root), check=True, **kwargs)


with china_mirror_environment(environment, options.cn_mirrors, gradle=False) as env:
    with mirrored_pub_lockfile(root, env):
        # 1. 构建 Go 原生核心（darwin c-shared dylib）
        run([sys.executable, str(root / 'scripts' / 'build_native.py'),
             '--platform', 'darwin', *variant.arguments], env=env)
        dylib = root / 'native' / 'build' / 'darwin' / 'libduanju_core.dylib'
        if not dylib.is_file():
            raise SystemExit('缺少原生库：' + str(dylib))

        # 2. 依赖与 CocoaPods
        run([flutter, 'pub', 'get', '--enforce-lockfile'], env=env)
        if shutil.which('pod'):
            run(['pod', 'install'], cwd=root / 'macos', env=env)

        # 3. 构建 macOS 应用
        run([flutter, 'build', 'macos', '--release', '--no-pub',
             *variant.flutter_arguments], env=env)

        app = root / 'build' / 'macos' / 'Build' / 'Products' / 'Release' / 'duanju_app.app'
        if not app.is_dir():
            raise SystemExit('未生成 macOS 应用：' + str(app))

        # 4. 按版本写入显示名称
        info_plist = app / 'Contents' / 'Info.plist'
        with info_plist.open('rb') as stream:
            info = plistlib.load(stream)
        info['CFBundleDisplayName'] = variant.name
        info['CFBundleName'] = variant.slug
        with info_plist.open('wb') as stream:
            plistlib.dump(info, stream)

        # 5. 把原生库放进应用包（与可执行文件同目录，Dart 侧按该路径加载）
        macos_dir = app / 'Contents' / 'MacOS'
        shutil.copy2(dylib, macos_dir / 'libduanju_core.dylib')

        # 6. 重新 ad-hoc 签名，保持包内签名一致（自用分发；对外分发需正式签名与公证）
        if shutil.which('codesign'):
            run(['codesign', '--force', '--deep', '--sign', '-', str(app)], env=env)

        # 7. 打包
        run([sys.executable, str(root / 'scripts' / 'package_release.py'),
             '--platform', 'macos', *variant.arguments], env=env)
