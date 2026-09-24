package protocol

// TypeHibernate asks a worker to stop its idle agent while keeping the saved
// conversation resumable. HibernateIdleMillis zero means an explicit request;
// a positive value makes the worker re-check that it has been detached and
// quiet for at least that long, since only it knows its live output clock.
const TypeHibernate = "session.hibernate"
