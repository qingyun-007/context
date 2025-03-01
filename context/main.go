package main

import (
	"errors"
	"internal/reflectlite"
	"sync"
	"sync/atomic"
	"time"
)

// ctx core 总共分为两大类
// 带cancel的。带cancel的为链式
// 不带cancel的。 不带cancel的为单点

// 带cancel的。首先声明。需要绑定一个头。cancelctx 作为第一个儿子
// 需要和parentctx绑定。 绑定完了之后。将心脏也就是取消函数交出。
// 和parentctx绑定。这个时候有三种情况
// case1 parentctx是一个cancelctx。这样的话。直接将cancelctx 存入parentctx就可以了
// case2 parentctx是一个afterfuncctx。这种情况。cancelctx也要升级为afterfuncctx
// case3 parentctx是一个普通的ctx。这种情况就是强绑定了。 搞一个select绑定

// 这是提前定义好了一个取消错误。用于处理err的
var Canceled = errors.New("context canceled")

// Context
// 1.Context
// Context context四件套。实现了这个方法的就是context
// 根Ctx。 cancelCtx withoutCancelCtx timerCtx afterFuncCtx valueCtx emptyCtx 都是对根ctx的继承
// 其中 除了withoutcancel 以及 valuectx 其他的都是继承自cancelCtx cancelCtx继承根ctx
// 像withdeadline之类的都是对cancel的功能封装

// Context主要是为了管理协程的声明周期。所以有Done() Err() 就可以了 Deadline 以及 Value是附加功能
// 因为ctx的核心功能就是这两个。只要用好多态特性。ctx不需要干嘛就err 以及 done。其他操作都再cancel 或其他func那
// 这就可以很好的利用多态的特性。只要封装好不同的函数就可以了。不需要不同形态的ctx返回不同的类型
type Context interface {
	Deadline() (deadline time.Time, ok bool)
	Done() <-chan struct{}
	Err() error
	Value(key any) any
}

// emptyCtx 空context
// 空ctx。contexts可以理解成一棵树。最顶上的context肯定是一个空白的context
type emptyCtx struct{}

func (emptyCtx) Deadline() (deadline time.Time, ok bool) {
	return
}

func (emptyCtx) Done() <-chan struct{} {
	return nil
}

func (emptyCtx) Err() error {
	return nil
}

func (emptyCtx) Value(key any) any {
	return nil
}

type backgroundCtx struct{ emptyCtx }
type todoCtx struct{ emptyCtx }

func (backgroundCtx) String() string {
	return "context.Background"
}
func (todoCtx) String() string {
	return "context.TODO"
}

func Background() Context {
	return backgroundCtx{}
}
func TODO() Context {
	return todoCtx{}
}

// CancelFunc
// 4.CancelFunc 取消函数是一个类型
type CancelFunc func()
type CancelCauseFunc func(cause error)

func WithCancel(parent Context) (ctx Context, cancel CancelFunc) {
	// 设想一下 parent是最顶端的。他返回了一个context。那么只能是他的子context了
	// 以及往取消这个子context的函数
	// 所以这个功能是声明一个带取消功能的context对象
	c := withCancel(parent)
	// 连接器没有cancel功能。就自己往cancelcontext对象添加傻瓜式cancel功能。上层执行cancel
	// 下面就执行 cancel(true,canceld.nil)
	return c, func() { c.cancel(true, Canceled, nil) }
}

// WithCancelCause只是将心脏。也就是取消的那个函数多接收一个cause
func WithCancelCause(parent Context) (ctx Context, cancel CancelCauseFunc) {
	c := withCancel(parent)
	return c, func(cause error) { c.cancel(true, Canceled, cause) }
}

func withCancel(parent Context) *cancelCtx {
	if parent == nil {
		panic("cannot create context from nil parent")
	}
	c := &cancelCtx{}
	// 上面以及声明好了对象了。下面这个只能是将父context 与 子context链接
	// 这个链接器没有带cancel功能
	c.propagateCancel(parent, c)
	return c
}

// afterfunc 执行stop函数 主动停止context。再执行这个函数
func AfterFunc(ctx Context, f func()) (stop func() bool) {
	a := &afterFuncCtx{
		f: f,
	}
	a.cancelCtx.propagateCancel(ctx, a)
	return func() bool {
		stopped := false
		a.once.Do(func() {
			stopped = true
		})
		if stopped {
			a.cancel(true, Canceled, nil)
		}
		return stopped
	}
}

// 定义这个是为了让 ctx融合的时候。断言parent ctx的类型，好让子ctx能继承parentctx的func
type afterFuncer interface {
	AfterFunc(func()) func() bool
}

type afterFuncCtx struct {
	cancelCtx
	once sync.Once // either starts running f or stops f from running
	f    func()
}

// cancel 父子分离 执行函数
func (a *afterFuncCtx) cancel(removeFromParent bool, err, cause error) {
	a.cancelCtx.cancel(false, err, cause)
	if removeFromParent {
		removeChild(a.Context, a)
	}
	a.once.Do(func() {
		go a.f()
	})
}

type stopCtx struct {
	Context
	stop func() bool
}

// goroutines counts the number of goroutines ever created; for testing.
var goroutines atomic.Int32

// &cancelCtxKey is the key that a cancelCtx returns itself for.
var cancelCtxKey int

// 猜测这个函数是判断 parent context 是不是 cancelctx
func parentCancelCtx(parent Context) (*cancelCtx, bool) {
	done := parent.Done()
	if done == closedchan || done == nil {
		return nil, false
	}
	p, ok := parent.Value(&cancelCtxKey).(*cancelCtx)
	if !ok {
		return nil, false
	}
	pdone, _ := p.done.Load().(chan struct{})
	if pdone != done {
		return nil, false
	}
	return p, true
}

// 父子分离
func removeChild(parent Context, child canceler) {
	if s, ok := parent.(stopCtx); ok {
		s.stop()
		return
	}
	p, ok := parentCancelCtx(parent)
	if !ok {
		return
	}
	p.mu.Lock()
	if p.children != nil {
		delete(p.children, child)
	}
	p.mu.Unlock()
}

// 因为许多ctx是对cancelctx的继承。父子分离的时候。child 不一定为cancelctx。可能为timectx或者afterfuncctx
// 但毫无意外。他们都需要分离。所以专门定义一个这样的接口。就不用判断了。
type canceler interface {
	cancel(removeFromParent bool, err, cause error)
	Done() <-chan struct{}
}

// TODO:不知道干什么的
var closedchan = make(chan struct{})

func init() {
	close(closedchan)
}

type cancelCtx struct {
	// context是为了继承context 的方法。好让cancelctx 可以被当作ctx传出去
	Context

	mu   sync.Mutex   // protects following fields
	done atomic.Value // of chan struct{}, created lazily, closed by first cancel call
	// children是为了链接children。parentctx cancel了。children也要跟着cancel
	children map[canceler]struct{} // set to nil by the first cancel call
	err      error                 // set to non-nil by the first cancel call
	cause    error                 // set to non-nil by the first cancel call
}

// context还有存储数据的功能
func (c *cancelCtx) Value(key any) any {
	if key == &cancelCtxKey {
		return c
	}
	return value(c.Context, key)
}

// 发送结束信号
func (c *cancelCtx) Done() <-chan struct{} {
	d := c.done.Load()
	if d != nil {
		return d.(chan struct{})
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	// TODO：这只是一个信号。为了节约空间。没有给他开辟空间
	// 要结束的时候。才给他开辟空间
	d = c.done.Load()
	if d == nil {
		d = make(chan struct{})
		c.done.Store(d)
	}
	return d.(chan struct{})
}

// 查看cancel context的错误
func (c *cancelCtx) Err() error {
	c.mu.Lock()
	err := c.err
	c.mu.Unlock()
	return err
}

// 融合取消。如果上层结束了。马上就执行下层的cancel
// cancel context 融合进context。注意这里是context 不是 cancel context。 context包含了cancel context
func (c *cancelCtx) propagateCancel(parent Context, child canceler) {
	c.Context = parent

	done := parent.Done()
	if done == nil {
		return // parent is never canceled
	}

	// 这里就像 mysql的驱动一样。如果接到了上层的取消信号。就没必要再链接了
	select {
	case <-done:
		// parent context 已经取消了。
		child.cancel(false, parent.Err(), Cause(parent))
		return
	default:
	}

	// 如果 parentcontext 是cancel context。就将子context 链接进 parent context
	if p, ok := parentCancelCtx(parent); ok {
		// 上锁防止。并发冲突
		p.mu.Lock()
		// 这个时候发现 parent context 出现了错误。上面出错了。下面赶紧取消
		if p.err != nil {
			// parent has already been canceled
			child.cancel(false, p.err, p.cause)
		} else {
			// parent context 没问题，就将子context 链接进去。返回
			if p.children == nil {
				p.children = make(map[canceler]struct{})
			}
			p.children[child] = struct{}{}
		}
		// 解锁返回
		p.mu.Unlock()
		return
	}

	// 如果 parentcontext 是afterCtx
	// cancelctx 升级为 afterctx
	if a, ok := parent.(afterFuncer); ok {
		// parent implements an AfterFunc method.
		c.mu.Lock()
		stop := a.AfterFunc(func() {
			child.cancel(false, parent.Err(), Cause(parent))
		})
		c.Context = stopCtx{
			Context: parent,
			stop:    stop,
		}
		c.mu.Unlock()
		return
	}

	// 不是上述两种情况的话. 子context没办法链接进 parent context。
	// 说明 parent context 是 todocontext
	// 开个后台监控这两个context什么时候取消
	// 这样的话。架构猜测：
	// 		1
	// 		1 child 在这
	// 2 2 2 2 2
	goroutines.Add(1)
	go func() {
		select {
		case <-parent.Done():
			child.cancel(false, parent.Err(), Cause(parent))
		case <-child.Done():
		}
	}()
}

type stringer interface {
	String() string
}

func contextName(c Context) string {
	// 这里把多态体现的淋漓尽致。 从context 转换成 stringer
	// 这样一个对象，就有可能实现两种接口的方法
	if s, ok := c.(stringer); ok {
		return s.String()
	}
	return reflectlite.TypeOf(c).String()
}

func (c *cancelCtx) String() string {
	return contextName(c.Context) + ".WithCancel"
}

// 真正的取消 cancel context的功能
func (c *cancelCtx) cancel(removeFromParent bool, err, cause error) {
	if err == nil {
		panic("context: internal error: missing cancel error")
	}
	if cause == nil {
		cause = err
	}

	// 加锁。防止并发冲突 比如。父ctx关闭了。他会遍历子ctx。如果这个时候子ctx也关闭了。就冲突了。所以加锁
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return // already canceled
	}
	c.err = err
	c.cause = cause
	// 这里 c.done 其实就是那个chan的空结构体的信号隧道
	d, _ := c.done.Load().(chan struct{})
	if d == nil {
		c.done.Store(closedchan)
	} else {
		close(d)
	}

	// 上层context已经取消了。 下层context 也跟着取消
	for child := range c.children {
		// NOTE: acquiring the child's lock while holding parent's lock.
		child.cancel(false, err, cause)
	}

	// 取消所有孩子
	c.children = nil
	c.mu.Unlock()

	// 如果不是从parent context 取消的。
	// 而就是这一个context 取消的。那么将这个context 与 parent context 分离
	if removeFromParent {
		removeChild(c.Context, c)
	}
}

// 这个和 withcancel 取反
// 结构为:
//
//	1
//
// 2 2 2 2 2 2
func WithoutCancel(parent Context) Context {
	if parent == nil {
		panic("cannot create context from nil parent")
	}
	return withoutCancelCtx{parent}
}

type withoutCancelCtx struct {
	c Context
}

func (withoutCancelCtx) Deadline() (deadline time.Time, ok bool) {
	return
}

func (withoutCancelCtx) Done() <-chan struct{} {
	return nil
}

func (withoutCancelCtx) Err() error {
	return nil
}

func (c withoutCancelCtx) Value(key any) any {
	return value(c, key)
}

func (c withoutCancelCtx) String() string {
	return contextName(c.c) + ".WithoutCancel"
}

// 自己封装了一个exceed错误
var DeadlineExceeded error = deadlineExceededError{}

type deadlineExceededError struct{}

func (deadlineExceededError) Error() string   { return "context deadline exceeded" }
func (deadlineExceededError) Timeout() bool   { return true }
func (deadlineExceededError) Temporary() bool { return true }

// 5.WithDeadLine。返回一个 context 以及 cancel。如果到时间了。ch会自动接到信号
func WithDeadline(parent Context, d time.Time) (Context, CancelFunc) {
	return WithDeadlineCause(parent, d, nil)
}

// 携带截止日期的cancelcontext
func WithDeadlineCause(parent Context, d time.Time, cause error) (Context, CancelFunc) {
	if parent == nil {
		panic("cannot create context from nil parent")
	}

	// parent context 是否存在截止日期。如果parent的截止日期在 子context 之前。就没有必要了
	if cur, ok := parent.Deadline(); ok && cur.Before(d) {
		// The current deadline is already sooner than the new one.
		return WithCancel(parent)
	}

	c := &timerCtx{
		deadline: d,
	}

	// 将子context 融进parent context
	c.cancelCtx.propagateCancel(parent, c)

	// 检查到截止日期还有多久
	dur := time.Until(d)
	// 如果已经到期了。就直接取消了
	if dur <= 0 {
		c.cancel(true, DeadlineExceeded, cause) // deadline has already passed
		return c, func() { c.cancel(false, Canceled, nil) }
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	// 如果timeCtx没有问题的话。时间到期之后。执行取消函数
	if c.err == nil {
		// time.AfterFunc()函数会在后台开个协程计时。到时了之后自动取消
		c.timer = time.AfterFunc(dur, func() {
			c.cancel(true, DeadlineExceeded, cause)
		})
	}
	return c, func() { c.cancel(true, Canceled, nil) }
}

// timeCtx是对 cancelCtx的继承
type timerCtx struct {
	cancelCtx
	timer *time.Timer // Under cancelCtx.mu.

	deadline time.Time
}

func (c *timerCtx) Deadline() (deadline time.Time, ok bool) {
	return c.deadline, true
}

func (c *timerCtx) String() string {
	return contextName(c.cancelCtx.Context) + ".WithDeadline(" +
		c.deadline.String() + " [" +
		time.Until(c.deadline).String() + "])"
}

func (c *timerCtx) cancel(removeFromParent bool, err, cause error) {
	// 调用从cancelcontext继承的取消
	c.cancelCtx.cancel(false, err, cause)

	// 父子分离
	if removeFromParent {
		// Remove this timerCtx from its parent cancelCtx's children.
		removeChild(c.cancelCtx.Context, c)
	}

	c.mu.Lock()
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	c.mu.Unlock()
}

// withtimeout 就是将现在的时间 加上 超时的时间。 变成了截止日期
func WithTimeout(parent Context, timeout time.Duration) (Context, CancelFunc) {
	return WithDeadline(parent, time.Now().Add(timeout))
}

func WithTimeoutCause(parent Context, timeout time.Duration, cause error) (Context, CancelFunc) {
	return WithDeadlineCause(parent, time.Now().Add(timeout), cause)
}

// context kv存储功能
func WithValue(parent Context, key, val any) Context {
	if parent == nil {
		panic("cannot create context from nil parent")
	}
	if key == nil {
		panic("nil key")
	}
	// TODO:如果key不是可以比较类型。为什么不能存储
	if !reflectlite.TypeOf(key).Comparable() {
		panic("key is not comparable")
	}
	return &valueCtx{parent, key, val}
}

type valueCtx struct {
	Context
	key, val any
}

// stringify tries a bit to stringify v, without using fmt, since we don't
// want context depending on the unicode tables. This is only used by
// *valueCtx.String().
func stringify(v any) string {
	switch s := v.(type) {
	case stringer:
		return s.String()
	case string:
		return s
	case nil:
		return "<nil>"
	}
	return reflectlite.TypeOf(v).String()
}

func (c *valueCtx) String() string {
	return contextName(c.Context) + ".WithValue(" +
		stringify(c.key) + ", " +
		stringify(c.val) + ")"
}

func (c *valueCtx) Value(key any) any {
	// 对的上。就返回 存储的value
	if c.key == key {
		return c.val
	}
	// 对不上就把各类型都试一遍。如果还是不是。就返回个空
	return value(c.Context, key)
}

func value(c Context, key any) any {
	for {
		switch ctx := c.(type) {
		case *valueCtx:
			if key == ctx.key {
				return ctx.val
			}
			c = ctx.Context
		case *cancelCtx:
			if key == &cancelCtxKey {
				return c
			}
			c = ctx.Context
		case withoutCancelCtx:
			if key == &cancelCtxKey {
				// This implements Cause(ctx) == nil
				// when ctx is created using WithoutCancel.
				return nil
			}
			c = ctx.c
		case *timerCtx:
			if key == &cancelCtxKey {
				return &ctx.cancelCtx
			}
			c = ctx.Context
		case backgroundCtx, todoCtx:
			return nil
		default:
			return c.Value(key)
		}
	}
}

// Cause 查看这个context被取消的原因
func Cause(c Context) error {
	if cc, ok := c.Value(&cancelCtxKey).(*cancelCtx); ok {
		cc.mu.Lock()
		defer cc.mu.Unlock()
		return cc.cause
	}
	return c.Err()
}
