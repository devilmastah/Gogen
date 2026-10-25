'use strict';
// Checks the pure download helpers in editors/vscode/binaryCore.js.
// The VS Code prompts stay in binary.js and are not loaded here.
// Run: node scripts/test_vscode_binary.js
const path = require('node:path');
const { assetFor, checksumMatches } = require(path.join(__dirname, '..', 'editors', 'vscode', 'binaryCore'));

const hash = 'a'.repeat(64);
let failed = 0;

function check(name, got, want) {
  if (got !== want) {
    console.error(name + ': got ' + JSON.stringify(got) + ', want ' + JSON.stringify(want));
    failed++;
  }
}

const assets = [
  ['win32', 'x64', 'gogen-windows-amd64.exe'],
  ['win32', 'arm64', ''],
  ['linux', 'x64', 'gogen-linux-amd64'],
  ['linux', 'arm64', 'gogen-linux-arm64'],
  ['darwin', 'x64', 'gogen-darwin-amd64'],
  ['darwin', 'arm64', 'gogen-darwin-arm64'],
  ['linux', 'ia32', ''],
  ['freebsd', 'x64', ''],
];
for (const [platform, arch, want] of assets) {
  check('assetFor ' + platform + ' ' + arch, assetFor(platform, arch), want);
}

const listed = hash + '  gogen-linux-amd64\n' + hash.toUpperCase() + ' *gogen-darwin-arm64\n';
check('checksum space form', checksumMatches(listed, 'gogen-linux-amd64', hash), true);
check('checksum star form', checksumMatches(listed, 'gogen-darwin-arm64', hash), true);
check('checksum wrong name', checksumMatches(listed, 'gogen-linux-arm64', hash), false);
check('checksum wrong hash', checksumMatches(listed, 'gogen-linux-amd64', 'b'.repeat(64)), false);
check('checksum short hash', checksumMatches(listed, 'gogen-linux-amd64', 'abc'), false);
check('checksum empty file', checksumMatches('', 'gogen-linux-amd64', hash), false);

if (failed > 0) {
  console.error(failed + ' check(s) failed');
  process.exit(1);
}
console.log('test_vscode_binary: ok');
