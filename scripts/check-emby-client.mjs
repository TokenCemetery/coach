// Execute the reference installation's AMD ApiClient, fetchhelper and querystring
// in a minimal runtime. This checks HTTP interoperability, not browser rendering.
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { randomBytes, createHash } from 'node:crypto';
import { once } from 'node:events';
import { mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import net from 'node:net';
import os from 'node:os';
import path from 'node:path';
import vm from 'node:vm';

const reference = new URL(process.env.EMBY_REFERENCE || 'http://192.168.1.11:8096/');
const binary = path.resolve(process.env.COACH_BINARY || 'bin/coach');
const webDirectory = process.env.COACH_WEB_DIR ? path.resolve(process.env.COACH_WEB_DIR) : undefined;
const password = randomBytes(32).toString('hex');
const data = await mkdtemp(path.join(os.tmpdir(), 'coach-client-check-'));
const quiet = { log() {}, error() {}, warn() {}, debug() {} };
let origin;
let child;
const paths = new Set();
const pending = new Set();
const sources = {};
let mediaDirectory;

async function createMedia() {
  mediaDirectory = path.join(data, 'movies');
  await mkdir(mediaDirectory);
  const movie = path.join(mediaDirectory, 'Sample.mp4');
  const encoder = spawn('ffmpeg', ['-v', 'error', '-nostdin', '-f', 'lavfi', '-i', 'color=c=black:s=64x64:r=10',
    '-f', 'lavfi', '-i', 'anullsrc=r=48000:cl=stereo', '-t', '1', '-c:v', 'mpeg4', '-c:a', 'aac', '-threads', '1', movie],
    { stdio: ['ignore', 'ignore', 'ignore'], timeout: 15000 });
  assert.equal((await once(encoder, 'exit'))[0], 0, 'generate synthetic movie');
  await writeFile(path.join(mediaDirectory, 'Broken.mkv'), 'not a video');
  return createHash('sha256').update(await readFile(movie)).digest('hex');
}

async function source(relative) {
  const response = await fetch(new URL(relative, origin), { signal: AbortSignal.timeout(15000), redirect: 'error' });
  assert.equal(response.status, 200, `reference module ${relative}`);
  const text = await response.text();
  sources[relative] = createHash('sha256').update(text).digest('hex');
  return text;
}

function loadAMD(code, dependencies) {
  const exports = {};
  vm.runInNewContext(code, {
    define(names, factory) {
      factory(...names.map(name => {
        if (name === 'exports') return exports;
        assert.ok(name in dependencies, `missing AMD dependency ${name}`);
        return dependencies[name];
      }));
    },
    URL, URLSearchParams, AbortSignal, AbortController, setTimeout, clearTimeout,
    console: quiet,
    fetch(url, options) {
      const target = new URL(url);
      assert.equal(target.origin, origin, 'client request must stay on the temporary Go server');
      paths.add(`${options?.method || 'GET'} ${target.pathname}`);
      const promise = fetch(target, { ...options, redirect: 'error' });
      pending.add(promise);
      promise.then(() => pending.delete(promise), () => pending.delete(promise));
      return promise;
    },
  }, { timeout: 2000 });
  return exports;
}

async function start() {
  const args = ['-data', data, '-listen', new URL(origin).host,
    ...(webDirectory ? ['-web-dir', webDirectory] : ['-web-upstream', reference.origin])];
  if (mediaDirectory) args.push('-media-dir', mediaDirectory);
  child = spawn(binary, args, { stdio: ['ignore', 'ignore', 'pipe'] });
  child.stderr.resume();
  for (let i = 0; i < 400; i++) {
    if (child.exitCode !== null) throw new Error('Go server exited before becoming ready');
    try { if ((await fetch(`${origin}/healthz`)).ok) return; } catch {}
    await new Promise(resolve => setTimeout(resolve, 50));
  }
  throw new Error('Go server startup timed out');
}

async function stop() {
  await Promise.allSettled([...pending]);
  if (child && child.exitCode === null) {
    const done = once(child, 'exit');
    child.kill('SIGTERM');
    await done;
  }
}

try {
  const init = spawn(binary, ['-init', '-data', data, '-username', 'compatibility-test'], { stdio: ['pipe', 'ignore', 'pipe'] });
  init.stderr.resume();
  init.stdin.end(password + '\n');
  assert.equal((await once(init, 'exit'))[0], 0, 'initialize temporary account');
  const portReservation = net.createServer();
  portReservation.listen(0, '127.0.0.1');
  await once(portReservation, 'listening');
  origin = `http://127.0.0.1:${portReservation.address().port}`;
  await new Promise(resolve => portReservation.close(resolve));
  await start();

  const htmlResponse = await fetch(`${origin}/web/index.html`);
  assert.equal(htmlResponse.status, 200);
  const referenceVersion = (await htmlResponse.text()).match(/data-appversion="([^"]+)"/)?.[1];
  assert.equal(referenceVersion, '4.10.0.40', 'reference frontend version changed; review the contract');

  const query = loadAMD(await source('web/modules/common/querystring.js'), {});
  const fetchHelper = loadAMD(await source('web/modules/emby-apiclient/fetchhelper.js'), { './../common/querystring.js': query });
  const cache = new Map();
  const locator = {
    appStorage: { getItem: key => cache.get(key), setItem: (key, value) => cache.set(key, value), removeItem: key => cache.delete(key) },
    appHost: { supports: () => false },
  };
  const { default: ApiClient } = loadAMD(await source('web/modules/emby-apiclient/apiclient.js'), {
    './events.js': { default: { trigger() {} } },
    './../common/servicelocator.js': locator,
    './../common/querystring.js': query,
    './../common/qualitydetection.js': { default: {} },
    './fetchhelper.js': fetchHelper,
  });
  function client(info) {
    const api = new ApiClient(origin, 'Emby Web', '4.10.0.40', 'Compatibility test', 'test-device', 1);
    api.enableAutomaticNetworking = false;
    if (info) api.serverInfo({ Id: info.Id, Name: info.ServerName });
    return api;
  }
  const api = client();
  const info = await api.getPublicSystemInfo();
  assert.equal(info.Version, '4.10.0.40');
  api.serverInfo({ Id: info.Id, Name: info.ServerName });
  const users = await api.getPublicUsers();
  assert.equal(users.length, 1);
  // onAuthenticated receives (instance, result) in this reference version.
  api.onAuthenticated = (instance, result) => {
    instance.setAuthenticationInfo({ UserId: result.User.Id, AccessToken: result.AccessToken });
    return Promise.resolve();
  };
  const auth = await api.authenticateUserByName('compatibility-test', password);
  const user = await api.getUser(auth.User.Id, false);
  assert.equal(user.Id, auth.User.Id);
  const views = await api.getUserViews({}, user.Id);
  assert.equal(views.TotalRecordCount, 0);
  assert.equal(views.Items.length, 0);
  await api.updatePartialDisplayPreferences({ theme: 'dark' }, user.Id);
  assert.equal((await api.getDisplayPreferences(user.Id)).theme, 'dark');
  await api.reportCapabilities({ PlayableMediaTypes: ['Video'], SupportedCommands: [] });

  await stop();
  await start();
  const restored = client(info);
  restored.setAuthenticationInfo({ UserId: user.Id, AccessToken: auth.AccessToken });
  assert.equal((await restored.getPublicSystemInfo()).Id, info.Id);
  assert.equal((await restored.getUser(user.Id, false)).Id, user.Id);
  assert.equal((await restored.getDisplayPreferences(user.Id)).theme, 'dark');
  const mediaChecks = [];
  if (process.env.COACH_CHECK_MEDIA === '1') {
    await stop();
    const sourceHash = await createMedia();
    await start();
    const mediaClient = client(info);
    mediaClient.setAuthenticationInfo({ UserId: user.Id, AccessToken: auth.AccessToken });
    const movieViews = await mediaClient.getUserViews({}, user.Id);
    assert.equal(movieViews.Items.length, 1);
    assert.equal(movieViews.Items[0].CollectionType, 'movies');
    const libraryId = movieViews.Items[0].Id;
    const movies = await mediaClient.getItems(user.Id, { ParentId: libraryId, IncludeItemTypes: 'Movie', Recursive: true, SortBy: 'SortName', Limit: 10 });
    assert.equal(movies.TotalRecordCount, 1, 'broken media must be skipped');
    const movieId = movies.Items[0].Id;
    const movie = await mediaClient.getItem(user.Id, movieId);
    assert.equal(movie.Name, 'Sample');
    assert.equal(movie.RunTimeTicks, 10000000);
    assert.equal(movie.MediaStreams.find(s => s.Type === 'Video').Width, 64);
    assert.equal(movie.MediaStreams.find(s => s.Type === 'Audio').SampleRate, 48000);
    assert.equal(movie.Path, undefined, 'host path must not be exposed');
    assert.equal((await mediaClient.getItems(user.Id, { Recursive: true, SearchTerm: 'sam' })).TotalRecordCount, 1);
    assert.equal((await mediaClient.getItems(user.Id, { ParentId: libraryId, StartIndex: 1 })).Items.length, 0);
    assert.equal((await mediaClient.getLatestItems({ ParentId: libraryId, Limit: 1 }))[0].Id, movieId);
    // Exercise delivery and saved progress against Coach only; the reference
    // supplies static modules, never authentication or playback requests.
    const localAPI = (route, options = {}) => fetch(new URL(`/emby/${route}`, origin), {
      ...options, headers: { 'X-Emby-Token': auth.AccessToken, ...options.headers },
      signal: AbortSignal.timeout(15000), redirect: 'error',
    });
    const profile = { DirectPlayProfiles: [{ Type: 'Video', Container: 'mp4', VideoCodec: 'mpeg4', AudioCodec: 'aac' }] };
    const playback = await mediaClient.getPlaybackInfo(movieId, { UserId: user.Id, EnableDirectStream: true, SubtitleStreamIndex: -1 }, profile);
    for (const [options, deviceProfile] of [
      [{ EnableDirectStream: false }, profile],
      [{ MaxStreamingBitrate: 1 }, profile],
      [{}, { ...profile, CodecProfiles: [{ Type: 'Video', Conditions: [{ Condition: 'LessThanEqual', Property: 'Width', Value: '32', IsRequired: false }] }] }],
      [{}, {}],
    ]) {
      const rejected = await mediaClient.getPlaybackInfo(movieId, options, deviceProfile);
      assert.equal(rejected.ErrorCode, 'NoCompatibleStream');
      assert.equal(rejected.MediaSources[0].SupportsDirectStream, false);
      assert.ok(!rejected.MediaSources[0].DirectStreamUrl, 'incompatible request must have no playback URL');
    }
    const mediaSource = playback.MediaSources[0];
    assert.equal(mediaSource.SupportsDirectStream, true);
    assert.equal(mediaSource.SupportsTranscoding, false);
    const streamURL = new URL(`/emby${mediaSource.DirectStreamUrl}`, origin);
    assert.equal(streamURL.origin, origin, 'stream must stay on Coach');
    const stream = await fetch(streamURL, { signal: AbortSignal.timeout(15000), redirect: 'error' });
    assert.equal(stream.status, 200);
    assert.equal(createHash('sha256').update(Buffer.from(await stream.arrayBuffer())).digest('hex'), sourceHash);
    const head = await fetch(streamURL, { method: 'HEAD', signal: AbortSignal.timeout(15000) });
    assert.equal(head.status, 200);
    assert.equal(Number(head.headers.get('content-length')), movie.Size);
    const range = await fetch(streamURL, { headers: { Range: 'bytes=16-31' }, signal: AbortSignal.timeout(15000) });
    assert.equal(range.status, 206);
    assert.equal(range.headers.get('content-range'), `bytes 16-31/${movie.Size}`);
    assert.deepEqual(Buffer.from(await range.arrayBuffer()), (await readFile(path.join(mediaDirectory, 'Sample.mp4'))).subarray(16, 32));
    const invalidRange = await fetch(streamURL, { headers: { Range: `bytes=${movie.Size}-` }, signal: AbortSignal.timeout(15000) });
    assert.equal(invalidRange.status, 416);
    await invalidRange.arrayBuffer();
    const withoutToken = new URL(streamURL);
    withoutToken.searchParams.delete('api_key');
    const unauthorizedStream = await fetch(withoutToken, { signal: AbortSignal.timeout(15000) });
    assert.equal(unauthorizedStream.status, 401);
    await unauthorizedStream.arrayBuffer();
    for (const event of ['Playing', 'Playing/Progress', 'Playing/Stopped']) {
      const report = await localAPI(`Sessions/${event}`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ ItemId: movieId, PositionTicks: 2500000 }),
      });
      assert.equal(report.status, 204);
    }
    await stop();
    await start();
    assert.equal((await mediaClient.getItem(user.Id, movieId)).Id, movieId);
    const resumed = await mediaClient.getItem(user.Id, movieId);
    assert.equal(resumed.UserData.PlaybackPositionTicks, 2500000);
    const resumeResponse = await localAPI(`Users/${user.Id}/Items/Resume`);
    assert.equal(resumeResponse.status, 200);
    assert.equal((await resumeResponse.json()).Items[0].Id, movieId);
    const finished = await localAPI('Sessions/Playing/Stopped', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ItemId: movieId, PositionTicks: 10000000 }),
    });
    assert.equal(finished.status, 204);
    const watched = await mediaClient.getItem(user.Id, movieId);
    assert.equal(watched.UserData.Played, true);
    assert.equal(watched.UserData.PlaybackPositionTicks, 0);
    assert.equal(createHash('sha256').update(await readFile(path.join(mediaDirectory, 'Sample.mp4'))).digest('hex'), sourceHash);
    mediaChecks.push('FFprobe video/audio metadata', 'movie views/list/detail', 'search/pagination/latest', 'broken file isolation', 'restart stable movie ID', 'source unchanged', 'original ApiClient PlaybackInfo query/profile negotiation', 'stream bytes/HEAD/Range/416/401', 'progress/resume after restart', 'watched at end');
  }
  await restored.logout();
  const denied = await fetch(`${origin}/emby/Users/${user.Id}`, { headers: { 'X-Emby-Token': auth.AccessToken } });
  assert.equal(denied.status, 401);
  console.log(JSON.stringify({ result: 'passed', referenceVersion, checks: ['static web assets', 'public info', 'public users', 'form login', 'query token', 'user', 'empty views', 'text/plain JSON settings', 'capabilities', 'restart identity/session/settings', 'logout revocation', ...mediaChecks], modulesSHA256: sources, requestPatterns: [...paths].map(p => p.replaceAll(user.Id, '{userId}').replace(/\/Items\/[a-f0-9]{32}/gi, '/Items/{itemId}')).sort() }, null, 2));
} finally {
  await stop();
  await rm(data, { recursive: true });
}
