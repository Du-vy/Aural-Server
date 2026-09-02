/**
 * Resolves the version from internal/buildinfo/buildinfo.go and validates it
 * against Semantic Versioning rules.
 *
 * Writes `version` and `tag` in GITHUB_OUTPUT format.
 */

import { readFileSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const ROOT = join(dirname(fileURLToPath(import.meta.url)), "..", "..");
const buildInfoPath = join(ROOT, "internal", "buildinfo", "buildinfo.go");

let content;
try {
  content = readFileSync(buildInfoPath, "utf8");
} catch (err) {
  console.error(`Failed to read ${buildInfoPath}: ${err.message}`);
  process.exit(1);
}

const match = content.match(/var\s+Version\s*=\s*"([^"]+)"/);
if (!match || !match[1]) {
  console.error("Could not find `var Version = \"...\"` in internal/buildinfo/buildinfo.go");
  process.exit(1);
}

const version = match[1].trim();

if (!/^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$/.test(version)) {
  console.error(`"${version}" is not a valid semantic version. Expected format: MAJOR.MINOR.PATCH(-PRERELEASE).`);
  process.exit(1);
}

// The regex above already rejects a leading `v`, so the version is the bare
// number and the tag is that number with the prefix this repo tags with.
console.log(`version=${version}`);
console.log(`tag=v${version}`);
