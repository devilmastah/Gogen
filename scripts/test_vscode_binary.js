'use strict';
// Checks the pure download helpers in editors/vscode/binaryCore.js.
// The VS Code prompts stay in binary.js and are not loaded here.
// Run: node scripts/test_vscode_binary.js
const path = require('node:path');
const {
  assetFor,
  checksumMatches,
  executableName,
  maxDownloadBytes,
  declaredTooLarge,
  appendCapped,
} = require(path.join(__dirname, '..', 'editors', 'vscode', 'binaryCore'));

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

check('executable unset', executableName(undefined), 'gogen');
check('executable empty global', executableName({ globalValue: '  ' }), 'gogen');
check('executable trimmed', executableName({ globalValue: '  gogen  ' }), 'gogen');
check('executable user path', executableName({ globalValue: 'C:\\gogen.exe', workspaceValue: '/tmp/x' }), 'C:\\gogen.exe');
check('executable workspace ignored', executableName({ workspaceValue: '/tmp/x', workspaceFolderValue: '/tmp/y' }), 'gogen');
check('executable non-string', executableName({ globalValue: 3 }), 'gogen');

check('download cap is 256MB', maxDownloadBytes, 256 * 1024 * 1024);
check('content-length missing', declaredTooLarge(undefined, 10), false);
check('content-length empty', declaredTooLarge('', 10), false);
check('content-length words', declaredTooLarge('nope', 10), false);
check('content-length under', declaredTooLarge('10', 10), false);
check('content-length over', declaredTooLarge('11', 10), true);
check('content-length list uses the first', declaredTooLarge(['11', '1'], 10), true);

const kept = [];
check('chunk within cap', appendCapped(kept, 0, Buffer.from('ab'), 4), 2);
check('chunk stored', Buffer.concat(kept).toString(), 'ab');
let threw = '';
try {
  appendCapped(kept, 2, Buffer.from('cdef'), 4);
} catch (err) {
  threw = err.message;
}
check('chunk over cap', threw, 'download is too large');
check('over-cap chunk was not stored', Buffer.concat(kept).toString(), 'ab');

if (failed > 0) {
  console.error(failed + ' check(s) failed');
  process.exit(1);
}
console.log('test_vscode_binary: ok');
